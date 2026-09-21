package capacity

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestCapacity(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Signals: Capacity Suite")
}
