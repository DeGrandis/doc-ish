package imaging

import (
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func slice(t *testing.T, i int) []byte {
	t.Helper()
	b, err := Synth{Patient: "PERSON^TEST", StudyUID: "1.2.3", SeriesUID: "1.2.3.1", Series: "AXIAL", SeriesNum: 2,
		Date: "20260901", Modality: "CT", BodyPart: "CHEST", Study: "CT CHEST", Slice: i, Size: 64}.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHeader(t *testing.T) {
	b := slice(t, 4)
	if !IsDICOM(b) {
		t.Fatal("not recognised as DICOM")
	}
	in, err := Header(b)
	if err != nil {
		t.Fatal(err)
	}
	if in.PatientName != "PERSON TEST" || in.StudyUID != "1.2.3" || in.SOPUID != "1.2.3.1.5" || in.StudyDate != "2026-09-01" ||
		in.InstanceNum != 5 || in.SeriesNum != 2 || in.Position != 20 || in.Rows != 64 || in.Title() != "CT scan, chest (CT CHEST)" {
		t.Fatalf("header: %+v (%s)", in, in.Title())
	}
	if _, err := Header([]byte("not dicom at all")); err == nil {
		t.Fatal("garbage parsed")
	}
}

// Compressed images go through gdcmconv and come out the same. Runs where
// GDCM is installed (the app image and the dev base image).
func TestDecodeCompressed(t *testing.T) {
	bin, err := exec.LookPath("gdcmconv")
	if err != nil {
		t.Skip("gdcmconv isn't installed here")
	}
	dir := t.TempDir()
	in, out := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
	os.WriteFile(in, slice(t, 3), 0o600)
	for _, mode := range []string{"--jpeg", "--j2k", "--rle"} {
		if b, err := exec.Command(bin, mode, "--lossless", in, out).CombinedOutput(); err != nil {
			if b, err = exec.Command(bin, mode, in, out).CombinedOutput(); err != nil {
				t.Fatalf("%s: %v %s", mode, err, b)
			}
		}
		comp, _ := os.ReadFile(out)
		if h, _ := Header(comp); native[h.Syntax] {
			t.Fatalf("%s: still uncompressed (%s)", mode, h.Syntax)
		}
		got, err := Decode(comp, 0, dir)
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		want, _ := Decode(slice(t, 3), 0, dir)
		for i := range want.Values {
			if got.Values[i] != want.Values[i] {
				t.Fatalf("%s: pixel %d is %v, want %v", mode, i, got.Values[i], want.Values[i])
			}
		}
	}
}

func TestDecodeRenderPack(t *testing.T) {
	p, err := Decode(slice(t, 0), 0, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p.Rows != 64 || p.Values == nil || p.Center != 40 || p.Width != 400 {
		t.Fatalf("pixels: %dx%d center %v width %v", p.Rows, p.Cols, p.Center, p.Width)
	}
	// The intercept is applied: air is -1000, tissue 40, the ring 700.
	if p.Min != -1000 || p.Max != 700 || p.Values[32*64+32] != 40 && p.Values[32*64+32] != -800 {
		t.Fatalf("values: min %v max %v centre %v", p.Min, p.Max, p.Values[32*64+32])
	}
	g := p.Render(0, 0).(*image.Gray)
	if g.GrayAt(0, 0).Y != 0 || g.GrayAt(32, 2).Y != 0 {
		t.Fatalf("air should be black: %v", g.GrayAt(0, 0))
	}
	meta, raw := p.Pack()
	if meta.Scale != 1 || meta.Offset != -1000 || len(raw) != 64*64*2 {
		t.Fatalf("pack: %+v %d", meta, len(raw))
	}
	small := p.Shrink(32)
	if small.Rows != 32 || len(small.Values) != 32*32 {
		t.Fatalf("shrink: %d", small.Rows)
	}
}
