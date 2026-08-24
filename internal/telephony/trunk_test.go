package telephony

import "testing"

// The switch's own table, verbatim from `sofia status` on the development
// host: tab separated, profiles and aliases mixed in with the gateways.
const sofiaStatus = "                     Name\t   Type\t                                      Data\tState\n" +
	"=========================================\n" +
	"            external-ipv6\tprofile\t    sip:mod_sofia@[fdfe:dcba:9876::1]:5080\tRUNNING (0)\n" +
	"            192.168.31.55\t  alias\t                                  internal\tALIASED\n" +
	"    external::example.com\tgateway\t                   sip:joeuser@example.com\tNOREG\n" +
	"   external::pstn_gateway\tgateway\t          sip:FreeSWITCH@192.168.31.5:5080\tNOREG\n" +
	"        external::carrier\tgateway\t                sip:aicc@carrier.example\tFAIL_WAIT\n" +
	"                 internal\tprofile\t          sip:mod_sofia@192.168.31.55:5060\tRUNNING (0)\n"

func TestTrunksReadsGatewaysAndLeavesTheProfilesAlone(t *testing.T) {
	got := parseTrunks(sofiaStatus)
	if len(got) != 3 {
		t.Fatalf("read %d trunks, want 3 — profiles and aliases are in the same "+
			"table and are not trunks: %+v", len(got), got)
	}
	if got[1].Name != "pstn_gateway" || got[1].Profile != "external" {
		t.Errorf("second trunk = %q on %q, want pstn_gateway on external",
			got[1].Name, got[1].Profile)
	}
	// NOREG is how an IP trunk is normally arranged — it never registers by
	// design. Calling that down would report a fault on every healthy
	// deployment, which is a worse answer than none.
	if !got[1].IsUp {
		t.Error("a NOREG trunk was reported down; it is dialable as it stands")
	}
	if got[2].IsUp || got[2].State != "FAIL_WAIT" {
		t.Errorf("third trunk = up=%v state=%q, want down and the switch's own word",
			got[2].IsUp, got[2].State)
	}
}
