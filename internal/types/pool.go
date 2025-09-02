package types

type Key struct {
	ID      string
	Name    string
	Value   string // secret
	Weight  int
	Enabled bool
}
