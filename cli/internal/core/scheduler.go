package core

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vercel/turborepo/cli/internal/util"

	"github.com/pyr-sh/dag"
)

const ROOT_NODE_NAME = "___ROOT___"

var errNoTask = errors.New("the given task has not been registered")

type Task struct {
	Name string
	// Deps are dependencies between tasks within the same package (e.g. `build` -> `test`)
	Deps util.Set
	// TopoDeps are dependencies across packages within the same topological graph (e.g. parent `build` -> child `build`) */
	TopoDeps util.Set
}

type Visitor = func(taskID string) error

type Scheduler struct {
	// TopologicGraph is a graph of workspaces
	TopologicGraph *dag.AcyclicGraph
	// TaskGraph is a graph of package-tasks
	TaskGraph *dag.AcyclicGraph
	// Tasks are a map of tasks in the scheduler
	Tasks            map[string]*Task
	PackageTaskDeps  [][]string
	rootEnabledTasks util.Set
}

// NewScheduler creates a new scheduler given a topologic graph of workspace package names
func NewScheduler(topologicalGraph *dag.AcyclicGraph) *Scheduler {
	return &Scheduler{
		Tasks:            make(map[string]*Task),
		TopologicGraph:   topologicalGraph,
		TaskGraph:        &dag.AcyclicGraph{},
		PackageTaskDeps:  [][]string{},
		rootEnabledTasks: make(util.Set),
	}
}

// SchedulerExecutionOptions are options for a single scheduler execution
type SchedulerExecutionOptions struct {
	// Packages in the execution scope, if nil, all packages will be considered in scope
	Packages []string
	// TaskNames in the execution scope, if nil, all tasks will be executed
	TaskNames []string
	// Restrict execution to only the listed task names
	TasksOnly bool
}

func (p *Scheduler) Prepare(options *SchedulerExecutionOptions) error {
	pkgs := options.Packages
	tasks := options.TaskNames
	if len(tasks) == 0 {
		// TODO(gsoltis): Is this behavior used?
		for key := range p.Tasks {
			tasks = append(tasks, key)
		}
	}

	if err := p.generateTaskGraph(pkgs, tasks, options.TasksOnly); err != nil {
		return err
	}

	return nil
}

// ExecOpts controls a single walk of the task graph
type ExecOpts struct {
	// Parallel is whether to run tasks in parallel
	Parallel bool
	// Concurrency is the number of concurrent tasks that can be executed
	Concurrency int
}

// Execute executes the pipeline, constructing an internal task graph and walking it accordingly.
func (p *Scheduler) Execute(visitor Visitor, opts ExecOpts) []error {
	var sema = util.NewSemaphore(opts.Concurrency)
	return p.TaskGraph.Walk(func(v dag.Vertex) error {
		// Always return if it is the root node
		if strings.Contains(dag.VertexName(v), ROOT_NODE_NAME) {
			return nil
		}
		// Acquire the semaphore unless parallel
		if !opts.Parallel {
			sema.Acquire()
			defer sema.Release()
		}
		return visitor(dag.VertexName(v))
	})
}

func (p *Scheduler) getTaskDefinition(pkg string, taskName string, taskID string) (*Task, error) {
	if task, ok := p.Tasks[taskID]; ok {
		return task, nil
	}
	if task, ok := p.Tasks[taskName]; ok {
		return task, nil
	}
	return nil, errNoTask
}

func (p *Scheduler) generateTaskGraph(pkgs []string, taskNames []string, tasksOnly bool) error {
	if p.PackageTaskDeps == nil {
		p.PackageTaskDeps = [][]string{}
	}

	packageTasksDepsMap := getPackageTaskDepsMap(p.PackageTaskDeps)

	traversalQueue := []string{}

	for _, pkg := range pkgs {
		isRootPkg := pkg == util.RootPkgName
		for _, taskName := range taskNames {
			if !isRootPkg || p.rootEnabledTasks.Includes(taskName) {
				taskID := util.GetTaskId(pkg, taskName)
				if _, err := p.getTaskDefinition(pkg, taskName, taskID); err != nil {
					// Initial, non-package tasks are not required to exist, as long as some
					// package in the list packages defines it as a package-task. Dependencies
					// *are* required to have a definition.
					continue
				}
				traversalQueue = append(traversalQueue, taskID)
			}
		}
	}
	if len(traversalQueue) == 0 {
		return fmt.Errorf("no tasks found to execute. Requested tasks %v in packages %v", strings.Join(taskNames, ", "), strings.Join(pkgs, ", "))
	}

	visited := make(util.Set)

	for len(traversalQueue) > 0 {
		taskId := traversalQueue[0]
		traversalQueue = traversalQueue[1:]
		pkg, taskName := util.GetPackageTaskFromId(taskId)
		if pkg == util.RootPkgName && !p.rootEnabledTasks.Includes(taskName) {
			return fmt.Errorf("%v needs an entry in turbo.json before it can be depended on because it is a task run from the root package", taskId)
		}
		task, err := p.getTaskDefinition(pkg, taskName, taskId)
		if err != nil {
			return err
		}
		if !visited.Includes(taskId) {
			visited.Add(taskId)
			deps := task.Deps

			if tasksOnly {
				deps = deps.Filter(func(d interface{}) bool {
					for _, target := range taskNames {
						return fmt.Sprintf("%v", d) == target
					}
					return false
				})
				task.TopoDeps = task.TopoDeps.Filter(func(d interface{}) bool {
					for _, target := range taskNames {
						return fmt.Sprintf("%v", d) == target
					}
					return false
				})
			}

			toTaskId := taskId
			hasTopoDeps := task.TopoDeps.Len() > 0 && p.TopologicGraph.DownEdges(pkg).Len() > 0
			hasDeps := deps.Len() > 0
			hasPackageTaskDeps := false
			if _, ok := packageTasksDepsMap[toTaskId]; ok {
				hasPackageTaskDeps = true
			}

			if hasTopoDeps {
				depPkgs := p.TopologicGraph.DownEdges(pkg)
				for _, from := range task.TopoDeps.UnsafeListOfStrings() {
					// add task dep from all the package deps within repo
					for depPkg := range depPkgs {
						fromTaskId := util.GetTaskId(depPkg, from)
						p.TaskGraph.Add(fromTaskId)
						p.TaskGraph.Add(toTaskId)
						p.TaskGraph.Connect(dag.BasicEdge(toTaskId, fromTaskId))
						traversalQueue = append(traversalQueue, fromTaskId)
					}
				}
			}

			if hasDeps {
				for _, from := range deps.UnsafeListOfStrings() {
					fromTaskId := util.GetTaskId(pkg, from)
					p.TaskGraph.Add(fromTaskId)
					p.TaskGraph.Add(toTaskId)
					p.TaskGraph.Connect(dag.BasicEdge(toTaskId, fromTaskId))
					traversalQueue = append(traversalQueue, fromTaskId)
				}
			}

			if hasPackageTaskDeps {
				if pkgTaskDeps, ok := packageTasksDepsMap[toTaskId]; ok {
					for _, fromTaskId := range pkgTaskDeps {
						p.TaskGraph.Add(fromTaskId)
						p.TaskGraph.Add(toTaskId)
						p.TaskGraph.Connect(dag.BasicEdge(toTaskId, fromTaskId))
						traversalQueue = append(traversalQueue, fromTaskId)
					}
				}
			}

			if !hasDeps && !hasTopoDeps && !hasPackageTaskDeps {
				p.TaskGraph.Add(ROOT_NODE_NAME)
				p.TaskGraph.Add(toTaskId)
				p.TaskGraph.Connect(dag.BasicEdge(toTaskId, ROOT_NODE_NAME))
			}
		}
	}
	return nil
}

func getPackageTaskDepsMap(packageTaskDeps [][]string) map[string][]string {
	depMap := make(map[string][]string)
	for _, packageTaskDep := range packageTaskDeps {
		from := packageTaskDep[0]
		to := packageTaskDep[1]
		if _, ok := depMap[to]; !ok {
			depMap[to] = []string{}
		}
		depMap[to] = append(depMap[to], from)
	}
	return depMap
}

func (p *Scheduler) AddTask(task *Task) *Scheduler {
	// If a root task is added, mark the task name as eligible for
	// root execution. Otherwise, it will be skipped.
	if util.IsPackageTask(task.Name) {
		pkg, taskName := util.GetPackageTaskFromId(task.Name)
		if pkg == util.RootPkgName {
			p.rootEnabledTasks.Add(taskName)
		}
	}
	p.Tasks[task.Name] = task
	return p
}

func (p *Scheduler) AddDep(fromTaskId string, toTaskId string) error {
	fromPkg, _ := util.GetPackageTaskFromId(fromTaskId)
	if fromPkg != ROOT_NODE_NAME && fromPkg != util.RootPkgName && !p.TopologicGraph.HasVertex(fromPkg) {
		return fmt.Errorf("found reference to unknown package: %v in task %v", fromPkg, fromTaskId)
	}
	p.PackageTaskDeps = append(p.PackageTaskDeps, []string{fromTaskId, toTaskId})
	return nil
}
