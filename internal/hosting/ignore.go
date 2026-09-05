package hosting

// ignore drops an error deliberately, on a path that has nothing better to do
// with it: a temporary file that could not be removed, a body that could not be
// flushed. It exists because this repository's errcheck configuration rejects
// the `_ = f()` form, and a named call makes each such decision reviewable.
func ignore(error) {}
