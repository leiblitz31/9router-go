### chore(repo): stop shipping the `sitegen` binary in the tree

The upstream merge carried `sitegen` — a 7.6 MB statically linked Go binary — in
as a tracked file. It is a build artifact of `tools/sitegen` (`make site`), so it
is untracked and ignored from here on rather than shipped on every checkout.