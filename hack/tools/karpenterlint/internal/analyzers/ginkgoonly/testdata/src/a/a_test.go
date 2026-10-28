package a

import "testing"

func RunSpecs(t *testing.T, name string) bool { return true }

func TestAPIs(t *testing.T) {
	RunSpecs(t, "Suite")
}

func TestComputeScore(t *testing.T) { // want `TestComputeScore is a plain go test`
	_ = t
}

func TestMain(m *testing.M) {}

func Testify(t *testing.T) {}

func BenchmarkScore(b *testing.B) {}

func FuzzScore(f *testing.F) {}
