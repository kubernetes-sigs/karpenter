package hashstructure

type Format uint

const FormatV2 Format = 2

type HashOptions struct{ SlicesAsSets bool }

func Hash(v interface{}, format Format, opts *HashOptions) (uint64, error) { return 0, nil }
