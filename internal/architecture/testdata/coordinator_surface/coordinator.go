package turn

type Coordinator struct {
	*coordinatorState
	text string
}

type coordinatorState struct {
	tasks  *int
	a, b   *int
	table  map[string]int
	Source func() int
}

type other struct{ tasks *int }

func (c *Coordinator) Exported()      {}
func (c Coordinator) Value()          {}
func (c *Coordinator) unexported()    {}
func (s *coordinatorState) Promoted() {}
func (o *other) Other()               {}
func Constructor() *Coordinator       { return nil }

func (c *Coordinator) checks(o *other) {
	if c.tasks == nil || nil != c.a {
	}
	if c.table == nil {
	}
	if c.Source != nil {
	}
	if o.tasks == nil {
	}
	if c.tasks == c.a {
	}
}

func (s *coordinatorState) more() {
	if s.b == nil {
	}
}

func (o *other) unrelated(c *Coordinator) {
	if c.tasks == nil {
	}
}

type view struct{ *Coordinator }

func (v view) Shown() {
	if v.tasks == nil {
	}
}

type step struct {
	c    *Coordinator
	name string
}

func (s *step) run() {
	c := s.c
	if c.tasks == nil || s.c.a == nil {
	}
	n := s.name
	_ = n
}

func (s *step) pair() {
	c, n := s.c, s.name
	if c.b != nil && n != "" {
	}
}
