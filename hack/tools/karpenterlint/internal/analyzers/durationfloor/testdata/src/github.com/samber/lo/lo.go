package lo

func Max[T ~int | ~int64](collection []T) T { return collection[0] }

func ToPtr[T any](x T) *T { return &x }
