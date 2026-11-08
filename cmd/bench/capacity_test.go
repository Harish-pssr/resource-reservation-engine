package main

import "testing"

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{5}, 5}, {[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5}}
	for _, c := range cases {
		if got := median(c.in); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
	in := []float64{3, 1, 2}
	median(in)
	if in[0] != 3 {
		t.Error("median must not reorder its input")
	}
}

func TestPercentChange(t *testing.T) {
	if got := pct(100, 120); got != 20 {
		t.Errorf("got %v", got)
	}
	if got := pct(100, 80); got != -20 {
		t.Errorf("got %v", got)
	}
	if got := pct(0, 5); got != 0 {
		t.Errorf("a zero baseline must not divide by zero: %v", got)
	}
}

func TestMeasurementRunsRefuseServersWithFaultsEnabled(t *testing.T) {
	if err := refuseIfFaultsEnabled([]byte(`{"faults_enabled":true}`), false); err == nil {
		t.Fatal("a throughput run must refuse a server with fault injection on")
	}
	if err := refuseIfFaultsEnabled([]byte(`{"faults_enabled":false}`), false); err != nil {
		t.Fatalf("a clean server is fine: %v", err)
	}
	if err := refuseIfFaultsEnabled([]byte(`{"faults_enabled":true}`), true); err != nil {
		t.Fatalf("the failure experiments may run with faults enabled: %v", err)
	}
	if err := refuseIfFaultsEnabled([]byte(`not json`), false); err == nil {
		t.Fatal("when the state cannot be read, refuse rather than guess")
	}
}
