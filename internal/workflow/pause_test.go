package workflow

import (
	"testing"
	"time"
)

const pauseYAML = `
version: 1
name: ship-it
triggers: [{type: manual}]
steps:
  - id: build
    image: alpine:3
    outputs: {tag: "json:tag"}
  - id: review
    needs: [build]
    approval:
      message: "Deploy ${{ steps.build.outputs.tag }} (${{ trigger.reason }})?"
    timeout: 72h
  - id: cool-off
    needs: [review]
    wait: 10m
  - id: deploy
    needs: [cool-off]
    image: alpine:3
    command: "echo ${{ steps.review.outputs.approved_by }} ${{ steps.review.outputs.comment }}"
`

// An approval and a wait are steps like any other: they take needs and
// when, an approval's outputs feed later steps, and an approval deadline
// is not capped by limits.timeout (it holds no container).
func TestPauseStepsValid(t *testing.T) {
	s, issues := parseT(t, pauseYAML, Options{MaxTimeout: 30 * time.Minute})
	if len(issues) > 0 {
		t.Fatal(issues)
	}
	if k := s.Step("review").Kind(); k != KindApproval || !s.Step("review").Pauses() {
		t.Fatal(k)
	}
	if k := s.Step("cool-off").Kind(); k != KindWait || !s.Step("cool-off").Pauses() || s.Step("build").Pauses() {
		t.Fatal(k)
	}
	if req, _ := TriggerFields(s); len(req) != 1 || req[0] != "reason" {
		t.Fatalf("an approval message's trigger fields are prompted for: %v", req)
	}
}

func TestPauseStepsInvalid(t *testing.T) {
	for _, tc := range []struct{ name, step, path, msg string }{
		{"two forms", "{id: aa, image: x, wait: 1m}", "/steps/0", "not both image and wait"},
		{"approval with command", "{id: aa, approval: {}, command: x}", "/steps/0/command", "not valid on approval steps"},
		{"approval with retry", "{id: aa, approval: {}, retry: 1}", "/steps/0/retry", "not valid on approval steps"},
		{"wait with outputs", "{id: aa, wait: 1m, outputs: {x: stdout}}", "/steps/0/outputs", "not valid on wait steps"},
		{"wait with timeout", "{id: aa, wait: 1m, timeout: 5m}", "/steps/0/timeout", "has no timeout"},
		{"zero wait", "{id: aa, wait: 0s}", "/steps/0/wait", "positive duration"},
		{"long wait", "{id: aa, wait: 721h}", "/steps/0/wait", "maximum wait"},
		{"secret in message", "{id: aa, approval: {message: '${{ secrets.token }}'}}", "/steps/0/approval/message", "secrets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, issues := parseT(t, "version: 1\nname: bad-one\nsteps:\n  - "+tc.step+"\n", Options{})
			mustHave(t, issues, tc.path, tc.msg, "")
		})
	}
	// A wait step has no outputs to reference; an approval has two.
	_, issues := parseT(t, `version: 1
name: bad-one
steps:
  - {id: ww, wait: 1m}
  - {id: rr, approval: {}}
  - {id: xx, needs: [ww, rr], image: x, command: "${{ steps.ww.outputs.a }} ${{ steps.rr.outputs.approver }}"}
`, Options{})
	mustHave(t, issues, "/steps/2/command", "wait step and has no outputs", "")
	mustHave(t, issues, "/steps/2/command", `declares no output "approver"`, "approved_by")
}
