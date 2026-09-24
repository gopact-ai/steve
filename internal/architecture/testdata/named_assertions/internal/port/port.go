package port

type Closer interface{ Close() error }

type Recorder interface{ Record() }

type Faked interface{ Fake() }

type Nilled interface{ Nil() }

type Empty interface{}

type Record struct{}

type local interface{ Name() string }

func Probe(v any) {
	if named, ok := v.(local); ok {
		_ = named.Name()
	}
	_, _ = v.(Empty)
	_, _ = v.(*Record)
}
