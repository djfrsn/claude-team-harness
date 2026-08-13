package persona

import "testing"

func TestRouteUsesMentionOwnerAndDefault(t *testing.T) {
	roster, err := NewSet([]Persona{
		{Name: "planner", DisplayName: "Planner", Default: true},
		{Name: "engineer", DisplayName: "Engineer"},
	})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	if got := roster.Route("@engineer check this", "").Name; got != "engineer" {
		t.Fatalf("mention route = %q, want engineer", got)
	}
	if got := roster.Route("follow up", "engineer").Name; got != "engineer" {
		t.Fatalf("owner route = %q, want engineer", got)
	}
	if got := roster.Route("new room", "").Name; got != "planner" {
		t.Fatalf("default route = %q, want planner", got)
	}
}
