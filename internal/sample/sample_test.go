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
	// No PPG characteristic exists on an Athena; optics is not published yet.
	if s.Has(PPG) {
		t.Error("Athena set contains PPG, which the headband does not have")
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
