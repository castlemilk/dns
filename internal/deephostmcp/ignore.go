package deephostmcp

// ignore drops an error deliberately, on a path that has nothing better to do
// with it. It exists because this repository's errcheck rejects the `_ = f()`
// form, and a named call makes each such decision reviewable.
func ignore(error) {}
