package e2e_test

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	kubectl "github.com/rancher-sandbox/ele-testhelpers/kubectl"
)

var _ = Describe("CAPI e2e tests", func() {
	//k := kubectl.New()
	Context("registration", func() {

		AfterEach(func() {
			kubectl.New().Delete("clusters", "-n", "default", "hello-kairos")
		})

		It("creates a simple capi definition", func() {
			err := kubectl.Apply("", "../../tests/fixtures/01_capi.yaml")
			Expect(err).ToNot(HaveOccurred())

			Eventually(func() string {
				b, _ := kubectl.GetData("default", "clusters", "hello-kairos", "jsonpath={.spec.infrastructureRef.kind}")
				return string(b)
			}, 2*time.Minute, 2*time.Second).Should(Equal("KairosCluster"))
			Eventually(func() string {
				b, _ := kubectl.GetData("default", "kairoscluster", "hello-kairos", "jsonpath={.spec.bootstrapToken}")
				return string(b)
			}, 2*time.Minute, 2*time.Second).ShouldNot(Equal(""))
		})
	})

})
