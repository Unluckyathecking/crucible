# Claim — cover `verifyWorkerSig` error branches (workers/sdk-go)

Seed commit. Directive in the PR body.

Target: `workers/sdk-go/crucible.go` `verifyWorkerSig` (~lines 161-207). Test-only, no
source change. Add a direct table test in `workers/sdk-go/crucible_test.go` exercising
the four currently-uncovered error branches: malformed header (present but missing
`t=`/`v1=`), non-numeric timestamp, **future**-skew timestamp (the `diff = -diff`
absolute-value arm), and bad-hex / wrong-length signature value.
