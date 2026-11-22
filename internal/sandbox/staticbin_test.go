package sandbox

import (
	"bytes"
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeELF writes a minimal little-endian ELF64 executable: one program
// header per type in progs and, with needed, a .dynamic section that names
// that shared library (DT_NEEDED). It has no code: only CheckStaticBin
// reads it.
func fakeELF(t *testing.T, path string, typ elf.Type, progs []elf.ProgType, needed string) {
	t.Helper()
	const ehsize, phsize, shsize = 64, 56, 64
	var body bytes.Buffer // what follows the program headers
	var sections []elf.Section64
	dataOff := uint64(ehsize + phsize*len(progs))
	if needed != "" {
		strtab := append([]byte{0}, needed+"\x00"...)
		strOff := dataOff
		body.Write(strtab)
		dynOff := dataOff + uint64(body.Len())
		for _, d := range []elf.Dyn64{{Tag: int64(elf.DT_NEEDED), Val: 1}, {Tag: int64(elf.DT_NULL)}} {
			_ = binary.Write(&body, binary.LittleEndian, d)
		}
		sections = []elf.Section64{
			{}, // SHN_UNDEF
			{Type: uint32(elf.SHT_STRTAB), Off: strOff, Size: uint64(len(strtab))},
			{Type: uint32(elf.SHT_DYNAMIC), Off: dynOff, Size: 32, Link: 1, Entsize: 16},
		}
	}
	shoff := uint64(0)
	if len(sections) > 0 {
		shoff = dataOff + uint64(body.Len())
	}
	h := elf.Header64{
		Type: uint16(typ), Machine: uint16(elf.EM_X86_64), Version: uint32(elf.EV_CURRENT),
		Phoff: ehsize, Shoff: shoff, Ehsize: ehsize,
		Phentsize: phsize, Phnum: uint16(len(progs)),
		Shentsize: shsize, Shnum: uint16(len(sections)),
	}
	copy(h.Ident[:], elf.ELFMAG)
	h.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	h.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	h.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, h)
	for _, p := range progs {
		_ = binary.Write(&b, binary.LittleEndian, elf.Prog64{Type: uint32(p), Flags: uint32(elf.PF_R)})
	}
	b.Write(body.Bytes())
	for _, s := range sections {
		_ = binary.Write(&b, binary.LittleEndian, s)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestCheckStaticBin(t *testing.T) {
	dir := t.TempDir()
	at := func(name string) string { return filepath.Join(dir, name) }

	fakeELF(t, at("static"), elf.ET_EXEC, []elf.ProgType{elf.PT_LOAD}, "")
	fakeELF(t, at("static-pie"), elf.ET_DYN, []elf.ProgType{elf.PT_LOAD, elf.PT_DYNAMIC}, "")
	for _, name := range []string{"static", "static-pie"} {
		if err := CheckStaticBin(at(name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}

	fakeELF(t, at("interp"), elf.ET_EXEC, []elf.ProgType{elf.PT_INTERP, elf.PT_LOAD}, "")
	fakeELF(t, at("needed"), elf.ET_EXEC, []elf.ProgType{elf.PT_LOAD}, "libc.so.6")
	fakeELF(t, at("object"), elf.ET_REL, nil, "")
	for name, data := range map[string]string{
		"script": "#!/bin/sh\n",
		"macho":  "\xcf\xfa\xed\xfe\x0c\x00\x00\x01", // a 64-bit Mach-O header's start
		"empty":  "",
	} {
		if err := os.WriteFile(at(name), []byte(data), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for name, want := range map[string]string{
		"interp":  "is dynamically linked (it needs a program interpreter)",
		"needed":  "is dynamically linked (it needs libc.so.6)",
		"object":  "is not an executable",
		"script":  "is not a Linux (ELF) binary",
		"macho":   "is not a Linux (ELF) binary",
		"empty":   "is not a Linux (ELF) binary",
		"missing": "does not exist",
	} {
		err := CheckStaticBin(at(name))
		if err == nil || !strings.Contains(err.Error(), at(name)+" "+want) {
			t.Errorf("%s: %v, want %q", name, err, want)
		}
	}

	msg := StaticBinError(CheckStaticBin(at("interp"))).Error()
	if !strings.Contains(msg, "must be a static Linux binary") || !strings.Contains(msg, "`"+BuildStatic+"`") || strings.Contains(msg, "\n") {
		t.Errorf("refusal %q", msg)
	}
}
