package util

type SWRRNode[T any] struct {
	Item        T
	Weight      int
	Current     int
	EffectiveWt int
}

type SWRR[T any] struct {
	nodes []*SWRRNode[T]
	total int
}

func NewSWRR[T any]() *SWRR[T] { return &SWRR[T]{} }

func (s *SWRR[T]) Add(item T, weight int) {
	if weight <= 0 {
		weight = 1
	}
	n := &SWRRNode[T]{Item: item, Weight: weight, EffectiveWt: weight}
	s.nodes = append(s.nodes, n)
	s.total += weight
}

func (s *SWRR[T]) Next() (T, bool) {
	var zero T
	if len(s.nodes) == 0 {
		return zero, false
	}
	var best *SWRRNode[T]
	for _, n := range s.nodes {
		n.Current += n.EffectiveWt
		if best == nil || n.Current > best.Current {
			best = n
		}
	}
	best.Current -= s.total
	return best.Item, true
}
