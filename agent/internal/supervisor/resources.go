package supervisor

// resourcePoint is one CPU sample for the hosted process.
type resourcePoint struct {
	proc  uint64
	total uint64
	ok    bool
}
