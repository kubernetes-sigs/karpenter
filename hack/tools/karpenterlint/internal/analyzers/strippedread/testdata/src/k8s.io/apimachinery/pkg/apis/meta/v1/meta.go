package v1

type ManagedFieldsEntry struct{ Manager string }

type ObjectMeta struct {
	Name          string
	ManagedFields []ManagedFieldsEntry
}

func (m *ObjectMeta) GetName() string                         { return m.Name }
func (m *ObjectMeta) GetManagedFields() []ManagedFieldsEntry  { return m.ManagedFields }
func (m *ObjectMeta) SetManagedFields(f []ManagedFieldsEntry) { m.ManagedFields = f }

type Object interface {
	GetName() string
	GetManagedFields() []ManagedFieldsEntry
}
