// SPDX-License-Identifier: Apache-2.0

// Package workflows holds this repository's workflows to go-tools' attestgate: what a release
// publishes is attested with its provenance first, in the same job.
package workflows_test

import (
	"testing"

	"github.com/spin-stack/go-tools/attestgate"
)

// Every step that publishes runs after an unconditional provenance attestation in its job; the
// ones that look like an upload and ship nothing anyone installs are named here, each with why.
func TestEveryUploadIsAttested(t *testing.T) {
	g := attestgate.Gate{Root: "..", NotPublished: map[string]string{
		"ci.yml/gate/Upload Go coverage HTML":                  "a coverage page for a person, kept seven days",
		"kernel.yml/build/Upload the kernel":                   "a lane's kernel for a person to fetch; a release builds its own",
		"lab.yml/lab/Keep the log":                             "a measurement's log, for a person",
		"qemu.yml/build/Upload the extracted binaries":         "a lane's QEMU for a person to fetch; a release builds its own",
		"qemu.yml/build/Publish the runtime image":             "what ci.yml's argument check pulls (task qemu:fetch); a release compiles QEMU, it never takes this",
		"report.yml/report/Keep the report":                    "the run's copy of report.json, for a person",
		"report.yml/report/Publish the report, and what moved": "report.json is measurement data: nothing installs or verifies it",
	}}
	if err := g.Check(); err != nil {
		t.Error(err)
	}
}
