package trace

import (
	"context"
	"testing"
)

func TestParse(t *testing.T) {
	good := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	c, ok := Parse(good)
	if !ok || c.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || c.SpanID != "00f067aa0ba902b7" || !c.Sampled {
		t.Fatalf("%+v %v", c, ok)
	}
	if c.String() != good {
		t.Error(c.String())
	}
	if c, ok := Parse("01-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00-future"); !ok || c.Sampled {
		t.Error("future version with extra field should parse, unsampled")
	}
	for _, bad := range []string{
		"", "garbage", "ff-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-00000000000000000000000000000000-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-0000000000000000-01",
		"00-4BF92F3577B34DA6A3CE929D0E0E4736-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01-extra",
		"0x-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
		"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-zz",
	} {
		if _, ok := Parse(bad); ok {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestStartAndContext(t *testing.T) {
	parent := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-00"
	c := Start(parent)
	if c.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || c.SpanID == "00f067aa0ba902b7" || c.Sampled {
		t.Errorf("child: %+v", c)
	}
	n := Start("nope")
	if !n.Valid() || len(n.TraceID) != 32 || len(n.SpanID) != 16 || !n.Sampled {
		t.Errorf("new: %+v", n)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Error("empty context")
	}
	got, ok := FromContext(WithContext(context.Background(), c))
	if !ok || got != c {
		t.Error("round trip")
	}
}
