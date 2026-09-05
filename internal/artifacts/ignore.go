package artifacts

// Excluded exposes the packer's ignore rules so a caller that receives files
// before they reach a directory (the folder-upload handler) can drop exactly
// what Pack would have dropped: any path under a `.git` component, `.npmrc`,
// `credentials`/`credentials.json`, `.env` and `.env.*`, and anything ending
// `.pem`, `.key`, `.p12` or `.pfx`.
//
// It is a thin wrapper rather than an edit to the copied packer, so re-copying
// artifacts.go from DeepHost stays a straight overwrite.
func Excluded(name string, directory bool) bool { return shouldExclude(name, directory) }
