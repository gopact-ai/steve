package turn

type Orchestrator struct {
	*orchestratorState
}

type orchestratorState struct {
	tasks *int
}

func (o *Orchestrator) Exported() {}

func (o *Orchestrator) checks() {
	if o.tasks == nil {
	}
}
