package shape

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestShape(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Signals: Shape Suite")
}
