package v1

type ObjectMeta struct {
	Name      string
	Namespace string
}

func (m *ObjectMeta) GetName() string { return m.Name }
