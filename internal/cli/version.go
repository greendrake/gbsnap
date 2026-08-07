package cli

// version is what "gbsnap -version" reports. Release builds stamp it with the
// contents of the VERSION file through -ldflags; a binary built any other way
// says so rather than claiming a release it is not.
var version = "dev"
