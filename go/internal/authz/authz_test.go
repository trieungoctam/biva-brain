package authz

import (
	"strings"
	"testing"
)

func TestNewToken(t *testing.T) {
	a, prefix, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := NewToken()
	if a == b {
		t.Fatal("hai token trùng nhau")
	}
	if !strings.HasPrefix(a, "biva_") || len(a) != len("biva_")+43 || !strings.HasPrefix(a, prefix) {
		t.Fatalf("token sai định dạng: %q prefix %q", a, prefix)
	}
}

func TestCanAccess(t *testing.T) {
	builder := Principal{Role: RoleBuilder, Operators: []string{"phuongnam"}}
	if !builder.CanAccessOperator("phuongnam") || builder.CanAccessOperator("hoanglong") || builder.CanAccessPlatform() {
		t.Error("builder: chỉ nhà xe được gán, không platform")
	}
	ops := Principal{Role: RoleOps}
	if !ops.CanAccessOperator("hoanglong") || ops.CanAccessPlatform() {
		t.Error("ops: mọi nhà xe, không platform")
	}
	lead := Principal{Role: RoleLead}
	if !lead.CanAccessOperator("hoanglong") || !lead.CanAccessPlatform() {
		t.Error("lead: mọi nhà xe và platform")
	}
	if _, err := ParseRole("admin"); err == nil {
		t.Error("role lạ phải bị từ chối")
	}
}
