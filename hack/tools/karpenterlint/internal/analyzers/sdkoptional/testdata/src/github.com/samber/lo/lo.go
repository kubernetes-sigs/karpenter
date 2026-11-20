package lo

func FromPtr[T any](x *T) T {
	if x == nil {
		var zero T
		return zero
	}
	return *x
}
