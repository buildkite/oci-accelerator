package main

// version identifies the build. Image builds set it from the VERSION file with
// -ldflags "-X main.version=...", and other builds report "dev".
var version = "dev"
