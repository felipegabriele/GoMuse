package sample

import "testing"

func TestClassicSetMatchesTheLegacyGlobals(t *testing.T) {
	s := Classic()

	got := s.All()
	want := All()
	if len(got) != len(want) {
		t.Fatalf("Classic().All() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("stream %d = %d, want %d", i, got[i], want[i])
		}
		if s.MetaFor(want[i]).NumChannels() != MetaFor(want[i]).NumChannels() {
			t.Errorf("channel count for %s differs between Set and global",
				MetaFor(want[i]).Name)
		}
	}
}

func TestAthenaSetShape(t *testing.T) {
	s := Athena()

	// 4 electrodes, not the classic 5: no Right AUX on this generation.
	if got := s.MetaFor(EEG).NumChannels(); got != 4 {
		t.Errorf("Athena EEG channels = %d, want 4", got)
	}
	if got := s.MetaFor(EEG).Rate; got != 256 {
		t.Errorf("Athena EEG rate = %v, want 256", got)
	}
	// No PPG characteristic exists on an Athena; Optics is its pulse sensor.
	if s.Has(PPG) {
		t.Error("Athena set contains PPG, which the headband does not have")
	}
	if !s.Has(Optics) {
		t.Fatal("Athena set is missing Optics, the headband's pulse sensor")
	}
	// Sixteen canonical channels whichever tag arrives: tags 0x34/0x35/0x36
	// carry 4, 8 or 16 into this one space, and the viewer is told the width
	// once in its hello message.
	if got := s.MetaFor(Optics).NumChannels(); got != 16 {
		t.Errorf("Athena optics channels = %d, want 16", got)
	}
	if got := s.MetaFor(Optics).Rate; got != 64 {
		t.Errorf("Athena optics rate = %v, want 64", got)
	}
	for _, id := range []StreamID{ACC, Gyro} {
		if !s.Has(id) {
			t.Errorf("Athena set is missing stream %d", id)
		}
		if got := s.MetaFor(id).NumChannels(); got != 3 {
			t.Errorf("Athena stream %d channels = %d, want 3", id, got)
		}
	}
}

func TestClassicSetHasNoOptics(t *testing.T) {
	// Optics and PPG are alternatives, not additions: publishing both would
	// tell a viewer to draw a pulse panel the headband cannot fill.
	if Classic().Has(Optics) {
		t.Error("Classic set contains Optics, which only the Athena produces")
	}
}

func TestSetAllIsInWireOrder(t *testing.T) {
	// Built out of order on purpose: the wire format depends on ascending
	// StreamID, not on the order the caller happened to list them.
	s := NewSet(metas[Gyro], metas[EEG], metas[ACC])

	want := []StreamID{EEG, ACC, Gyro}
	got := s.All()
	if len(got) != len(want) {
		t.Fatalf("All() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("All()[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func TestSetAllReturnsACopy(t *testing.T) {
	s := Classic()
	ids := s.All()
	if len(ids) == 0 {
		t.Fatal("no streams")
	}
	ids[0] = 99
	if s.All()[0] == 99 {
		t.Error("mutating the returned slice changed the Set")
	}
}

func TestSetMetaReportsAbsentStreams(t *testing.T) {
	s := Athena()
	if _, ok := s.Meta(PPG); ok {
		t.Error("Meta(PPG) reported present on an Athena")
	}
	defer func() {
		if recover() == nil {
			t.Error("MetaFor on an absent stream did not panic")
		}
	}()
	_ = s.MetaFor(PPG)
}

func TestNewSetRejectsDuplicates(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("NewSet accepted a duplicate stream id")
		}
	}()
	_ = NewSet(metas[EEG], metas[EEG])
}
