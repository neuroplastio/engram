module github.com/neuroplastio/engram/enlaunch

go 1.24

require github.com/neuroplastio/engram v0.0.0-20260924064618-eab89b44c5ec

// In this repository enlaunch builds against the root module beside it. A
// program that requires enlaunch gets the root at the version above: a pushed
// commit, raised with go mod edit -require when enlaunch needs a newer root.
replace github.com/neuroplastio/engram => ../
