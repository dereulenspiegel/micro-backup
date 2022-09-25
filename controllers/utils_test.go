package controllers

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("slice utils", func() {
	It("should remove specific element", func() {
		testSlice := []string{"one", "two", "three"}
		resultSlice := remove(testSlice, 1)
		Expect(resultSlice).NotTo(ContainElement("two"))
		Expect(resultSlice).To(ContainElement("one"))
		Expect(resultSlice).To(ContainElement("three"))
		Expect(resultSlice).To(HaveLen(2))
	})
})
