package consoleapi

type Extra interface {
	Referenced()
	Deeper
}

type Deeper interface{ Unused() }
