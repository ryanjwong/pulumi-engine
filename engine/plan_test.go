// Copyright 2026 Ryan Wong
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Offline: plan options are validated per operation kind before any engine
// work starts, and PlanViolation is a kind of its own.
func TestPlanOptionsValidation(t *testing.T) {
	spec := StackSpec{
		Name:    "dev",
		Project: ProjectSpec{Name: "plans", Dir: t.TempDir()},
		Backend: BackendSpec{URL: "file://" + t.TempDir()},
		Secrets: SecretsSpec{Provider: "b64"},
		Create:  true,
	}
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	noop := GoProgram(func(*pulumi.Context) error { return nil })

	cases := []struct {
		name  string
		start func() *Operation
		field string
	}{
		{"plan on preview", func() *Operation { return st.Preview(ctx, noop, Options{Plan: "x.json"}) }, "options.plan"},
		{"savePlan on up", func() *Operation { return st.Up(ctx, noop, Options{SavePlan: "x.json"}) }, "options.savePlan"},
		{"generatePlan on destroy", func() *Operation { return st.Destroy(ctx, nil, Options{GeneratePlan: true}) }, "options.savePlan"},
		{"plan and planJson", func() *Operation {
			return st.Up(ctx, noop, Options{Plan: "x.json", PlanJSON: json.RawMessage(`{}`)})
		}, "options.plan"},
		{"missing plan file", func() *Operation {
			return st.Up(ctx, noop, Options{Plan: filepath.Join(t.TempDir(), "nope.json")})
		}, "options.plan"},
		{"plan not JSON", func() *Operation { return st.Up(ctx, noop, Options{PlanJSON: json.RawMessage(`nope`)}) }, "options.plan"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			op := c.start()
			for range op.Events() {
			}
			_, err := op.Wait()
			var inv InvalidSpec
			if !errors.As(err, &inv) || inv.Field != c.field {
				t.Fatalf("expected InvalidSpec on %s, got %T: %v", c.field, err, err)
			}
		})
	}

	v := PlanViolation{Resources: []PlanViolationResource{{URN: "urn:a", Message: "update is not allowed by the plan"}}}
	if KindOf(v) != KindPlanViolation {
		t.Errorf("KindOf(PlanViolation) = %q", KindOf(v))
	}
	if !strings.Contains(v.Error(), "urn:a") {
		t.Errorf("message should name the resource: %q", v.Error())
	}
}

// Real providers: a preview generates a plan, an up constrained by that plan
// succeeds, and an up whose program diverged from the plan fails with
// PlanViolation naming the resource.
func TestIntegrationUpdatePlan(t *testing.T) {
	spec := integrationSpec(t, "plan", "")
	ctx := context.Background()
	st, err := Open(ctx, spec)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Remove(context.Background(), true) })

	program := func(length int) Program {
		return GoProgram(func(ctx *pulumi.Context) error {
			pet, err := random.NewRandomPet(ctx, "pet", &random.RandomPetArgs{Length: pulumi.Int(length)})
			if err != nil {
				return err
			}
			ctx.Export("petName", pet.ID())
			return nil
		})
	}

	planFile := filepath.Join(t.TempDir(), "plan.json")
	op := st.Preview(ctx, program(2), Options{SavePlan: planFile, GeneratePlan: true})
	drain(t, op)
	res, err := op.Wait()
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	if len(res.Plan) == 0 {
		t.Fatal("preview with GeneratePlan returned no plan")
	}
	fileContent, err := os.ReadFile(planFile)
	if err != nil {
		t.Fatalf("plan file: %v", err)
	}
	if string(fileContent) != string(res.Plan) {
		t.Errorf("Result.Plan and the SavePlan file differ")
	}
	var wire apitype.DeploymentPlanV1
	if err := json.Unmarshal(res.Plan, &wire); err != nil {
		t.Fatalf("plan is not a DeploymentPlanV1: %v", err)
	}
	if len(wire.ResourcePlans) < 2 { // the stack and the pet (plus the default provider)
		t.Errorf("plan has %d resource plans", len(wire.ResourcePlans))
	}
	// The manifest's version is the CLI binary's version string; a library
	// build has none, and neither the CLI nor the engine checks it.
	if wire.Manifest.Time.IsZero() {
		t.Errorf("plan manifest has no time: %+v", wire.Manifest)
	}

	// The plan proposes creates; an up with the same program is within it.
	op = st.Up(ctx, program(2), Options{Plan: planFile})
	drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("up --plan: %v", err)
	}
	if res.Changes["create"] < 2 {
		t.Errorf("up changes = %v", res.Changes)
	}

	// A new plan says "same" for everything; a program that now wants a
	// replacement (length is a replace-triggering input) exceeds it.
	op = st.Preview(ctx, program(2), Options{GeneratePlan: true})
	drain(t, op)
	res, err = op.Wait()
	if err != nil {
		t.Fatalf("second preview: %v", err)
	}
	op = st.Up(ctx, program(3), Options{PlanJSON: res.Plan})
	events := drain(t, op)
	res, err = op.Wait()
	var pv PlanViolation
	if !errors.As(err, &pv) {
		t.Fatalf("expected PlanViolation, got %T: %v (events %v)", err, err, eventTypes(events))
	}
	if len(pv.Resources) == 0 || !strings.HasSuffix(pv.Resources[0].URN, "::pet") {
		t.Errorf("violation should name the pet: %+v", pv.Resources)
	}
	if !strings.Contains(pv.Resources[0].Message, "plan") {
		t.Errorf("violation message: %q", pv.Resources[0].Message)
	}
	if KindOf(err) != KindPlanViolation {
		t.Errorf("KindOf = %q", KindOf(err))
	}
	if res.Changes["replace"] != 0 {
		t.Errorf("the violating replace must not have happened: %v", res.Changes)
	}
	// The pet is untouched.
	out, err := st.Outputs(ctx, false)
	if err != nil {
		t.Fatalf("outputs: %v", err)
	}
	if name, _ := out.Values["petName"].(string); strings.Count(name, "-") != 1 {
		t.Errorf("petName after the refused replace = %q", name)
	}

	op = st.Destroy(ctx, nil, Options{})
	drain(t, op)
	if _, err := op.Wait(); err != nil {
		t.Fatalf("destroy: %v", err)
	}
}

// CLI parity: a plan saved by the library is honoured (and enforced) by
// `pulumi up --plan`, and a plan saved by `pulumi preview --save-plan` is
// honoured (and enforced) by the library.
func TestIntegrationParityPlans(t *testing.T) {
	cli := newParityCLI(t)
	ctx := context.Background()

	t.Run("library saves, CLI applies", func(t *testing.T) {
		dir := copyFixture(t, "yaml-parity")
		spec := cli.spec("libplan", dir, true)
		spec.Config = map[string]ConfigValue{"petLength": {Value: "2"}, "apiKey": {Value: "k", Secret: true}}
		st, err := Open(ctx, spec)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = st.Remove(context.Background(), true) })

		planFile := filepath.Join(dir, "plan.json")
		op := st.Preview(ctx, localProgram(dir), Options{SavePlan: planFile})
		drain(t, op)
		if _, err := op.Wait(); err != nil {
			t.Fatalf("preview: %v", err)
		}
		cli.mustRun(t, dir, "up", "--stack", "libplan", "--plan", planFile, "--yes")

		// The library's plan now says "same"; the CLI refuses a replace.
		op = st.Preview(ctx, localProgram(dir), Options{SavePlan: planFile})
		drain(t, op)
		if _, err := op.Wait(); err != nil {
			t.Fatalf("second preview: %v", err)
		}
		cli.mustRun(t, dir, "config", "set", "petLength", "3", "--stack", "libplan")
		out, err := cli.run(dir, "up", "--stack", "libplan", "--plan", planFile, "--yes")
		if err == nil {
			t.Fatalf("pulumi up --plan should have refused the replace:\n%s", out)
		}
		if !strings.Contains(err.Error(), "violates plan") && !strings.Contains(err.Error(), "not allowed by the plan") {
			t.Errorf("CLI error should be a plan violation: %v", err)
		}
		var dep apitype.DeploymentV3
		_, dep = exportedDeployment(t, mustExport(t, st))
		if got := resourceNames(dep); !contains(got, "random:index/randomPet:RandomPet::pet") {
			t.Errorf("the refused update must leave the pet in place: %v", got)
		}
		cli.mustRun(t, dir, "destroy", "--stack", "libplan", "--yes")
	})

	t.Run("CLI saves, library applies", func(t *testing.T) {
		dir := copyFixture(t, "yaml-parity")
		cli.mustRun(t, dir, "stack", "init", "cliplan")
		cli.mustRun(t, dir, "config", "set", "petLength", "2", "--stack", "cliplan")
		cli.mustRun(t, dir, "config", "set", "--secret", "apiKey", "k", "--stack", "cliplan")
		planFile := filepath.Join(dir, "plan.json")
		cli.mustRun(t, dir, "preview", "--stack", "cliplan", "--save-plan", planFile)

		st, err := Open(ctx, cli.spec("cliplan", dir, false))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { _ = st.Remove(context.Background(), true) })
		op := st.Up(ctx, localProgram(dir), Options{Plan: planFile})
		drain(t, op)
		res, err := op.Wait()
		if err != nil {
			t.Fatalf("up with the CLI's plan: %v", err)
		}
		if res.Changes["create"] < 3 {
			t.Errorf("up changes = %v", res.Changes)
		}

		// A CLI plan of "same" steps; the library refuses a replace and
		// names the resource.
		cli.mustRun(t, dir, "preview", "--stack", "cliplan", "--save-plan", planFile)
		planBytes, err := os.ReadFile(planFile)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetConfig(ctx, "petLength", ConfigValue{Value: "3"}); err != nil {
			t.Fatal(err)
		}
		op = st.Up(ctx, localProgram(dir), Options{PlanJSON: planBytes})
		drain(t, op)
		_, err = op.Wait()
		var pv PlanViolation
		if !errors.As(err, &pv) {
			t.Fatalf("expected PlanViolation, got %T: %v", err, err)
		}
		if len(pv.Resources) == 0 || !strings.HasSuffix(pv.Resources[0].URN, "::pet") {
			t.Errorf("violation should name the pet: %+v", pv.Resources)
		}
		cli.mustRun(t, dir, "destroy", "--stack", "cliplan", "--yes")
	})
}

func mustExport(t *testing.T, st *Stack) []byte {
	t.Helper()
	raw, err := st.Export(context.Background())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	return raw
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
