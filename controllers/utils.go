package controllers

func remove[T any](sl []T, i int) []T {
	sl[i] = sl[len(sl)-1]
	return sl[:len(sl)-1]
}
