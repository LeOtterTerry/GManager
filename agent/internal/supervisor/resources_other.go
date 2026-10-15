//go:build !linux

package supervisor

func sampleResources(int, resourcePoint) (uint64, bool, float64, bool, resourcePoint) {
	return 0, false, 0, false, resourcePoint{}
}
