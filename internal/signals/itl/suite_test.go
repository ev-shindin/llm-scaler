package itl

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestITL(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Signals: ITL Suite")
}
