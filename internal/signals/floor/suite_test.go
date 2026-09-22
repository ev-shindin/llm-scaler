package floor

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestFloor(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Signals: Floor Suite")
}
