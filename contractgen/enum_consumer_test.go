package contractgen

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGeneratedConsumersShareAdmittedStringEnums(t *testing.T) {
	directory := t.TempDir()
	generateConsumer(t, directory, []projectionSample{
		{name: "review", schema: `{"$defs":{"Decision":{"type":"string","enum":["approved","denied"]}},"type":"object","properties":{"state":{"type":"string","$ref":"#/$defs/Decision","pattern":"^approved$"}},"required":["state"],"additionalProperties":false}`, wire: `{"state":"approved"}`},
		{name: "other", schema: `{"$defs":{"Decision":{"type":"string","enum":["queued","done"]}},"type":"string","$ref":"#/$defs/Decision","not":{"const":"done"}}`, wire: `"queued"`},
		{name: "exclusive", schema: `{"type":"string","oneOf":[{"enum":["approved","denied"]},{"const":"denied"}]}`, wire: `"approved"`},
		{name: "impossible", schema: `{"type":"string","enum":["denied"],"not":{"const":"denied"}}`},
		{name: "open", schema: `{"type":"string","anyOf":[{"const":"approved"},{"type":"string"}]}`, wire: `"invented"`},
		{name: "colliding", schema: `{"type":"string","enum":["a-b","a_b"]}`, wire: `"a-b"`},
	})
	goConsumer := `package consumer
import (
    "testing"
    "github.com/Tangerg/frame"
    "github.com/Tangerg/frame/value"
)
var _ interface{ JSONSchemaEnum() []string } = ReviewOutputState("")
var _ interface{ JSONSchemaEnum() []string } = OtherOutput("")
func TestEnums(t *testing.T) {
    capability, err := frame.ParseCapability([]byte(CapabilityJSON))
    if err != nil { t.Fatal(err) }
    review, _ := capability.Method(ReviewMethod)
    schema, err := value.SchemaFor[ReviewOutput]()
    if err != nil { t.Fatal(err) }
    projected, err := value.AdmitSchema(schema)
    if err != nil { t.Fatal(err) }
    for _, sample := range []struct{ wire string; admitted bool }{
        {"{\"state\":\"approved\"}",true},
        {"{\"state\":\"denied\"}",false},
        {"{\"state\":\"invented\"}",false},
    } {
        decoded := ReviewOutput{State:ReviewOutputStateApproved}
        for _, admission := range []error{review.ValidateOutput([]byte(sample.wire)), projected.Validate([]byte(sample.wire)), value.DecodeValue([]byte(sample.wire),&decoded,"review")} {
            if (admission==nil)!=sample.admitted { t.Fatalf("admission of %s: %v",sample.wire,admission) }
        }
        if decoded.State!=ReviewOutputStateApproved { t.Fatal("refusal changed its destination") }
    }
    members := OtherOutput("").JSONSchemaEnum()
    if len(members)!=1 || members[0]!=string(OtherOutputQueued) { t.Fatalf("scope changed: %q",members) }
    members[0]="invented"
    if OtherOutput("").JSONSchemaEnum()[0]!=string(OtherOutputQueued) { t.Fatal("vocabulary escaped its owner") }
    if len(CollidingOutput("").JSONSchemaEnum())!=2 || CollidingOutputAB==CollidingOutputAB2 { t.Fatal("colliding names changed the vocabulary") }
    var exclusive ExclusiveOutput
    if err:=value.DecodeValue([]byte("\"denied\""),&exclusive,"exclusive");err==nil { t.Fatal("overlapping oneOf branches escaped their contract") }
}
`
	test := filepath.Join(directory, "consumer_test.go")
	if err := os.WriteFile(test, []byte(goConsumer), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "go", "test", filepath.Join(directory, "types.go"), test)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("generated Go enum consumer: %s\n%v", output, err)
	}
	typeScriptConsumer := `import type {ReviewOutput,OtherOutput,ExclusiveOutput,ImpossibleOutput,OpenOutput} from "./methods.generated.js";
const review: ReviewOutput = {state:"approved"};
const other: OtherOutput = "queued";
const exclusive: ExclusiveOutput = "approved";
const open: OpenOutput = "invented";
// @ts-expect-error The original pattern excludes this enum member.
const invalidReview: ReviewOutput = {state:"denied"};
// @ts-expect-error The second method retains its own definition scope and negation.
const invalidOther: OtherOutput = "done";
// @ts-expect-error Matching both oneOf branches excludes this member.
const invalidExclusive: ExclusiveOutput = "denied";
// @ts-expect-error An empty vocabulary cannot admit its original candidate.
const invalidImpossible: ImpossibleOutput = "denied";
void [review,other,exclusive,open,invalidReview,invalidOther,invalidExclusive,invalidImpossible];
`
	if err := os.WriteFile(filepath.Join(directory, "consumer.ts"), []byte(typeScriptConsumer), 0600); err != nil {
		t.Fatal(err)
	}
	runTypeScriptConsumer(t, directory)
}
