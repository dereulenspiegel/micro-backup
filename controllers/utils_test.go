package controllers

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("Removing specific index from slice", func() {
	testSlice := []string{"one", "two", "three"}
	resultSlice := remove(testSlice, 1)
	Expect(resultSlice).NotTo(ContainElement("two"))
	Expect(resultSlice).To(ContainElement("one"))
	Expect(resultSlice).To(ContainElement("three"))
})
