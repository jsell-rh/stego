package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/jsell-rh/stego/internal/compiler"
	"github.com/jsell-rh/stego/internal/fill"
	"gopkg.in/yaml.v3"
)

// runFillSign implements `stego fill sign <name>` (ST-EXT-CLI03,
// ST-EXT-CMD03): it signs the digest of the fill's current evidence with the
// person's own SSH or GPG key, as their git configuration names it, and
// writes the sign-off into the fill manifest. It refuses a fill with no
// evidence or with stale evidence. STEGO holds no key: it makes, stores and
// uses none of its own.
func runFillSign(args []string) error {
	fs := flag.NewFlagSet("fill sign", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return fmt.Errorf("fill name is required: stego fill sign <name>")
	}
	name := fs.Arg(0)
	if _, err := fillPackageName(name); err != nil {
		return err
	}
	input, err := buildReconcilerInput()
	if err != nil {
		return err
	}
	check, err := compiler.ResolveFillCheck(input)
	if err != nil {
		return err
	}
	dir := filepath.Join(input.ProjectDir, "fills", name)
	manifest := filepath.Join(dir, fill.ManifestFile)
	f, err := fill.ReadManifest(manifest, check)
	if err != nil {
		return err
	}
	if f.Evidence == nil {
		return fmt.Errorf("fill %q has no evidence: run stego test first", name)
	}
	current, err := fill.CurrentState(dir, f, check)
	if err != nil {
		return err
	}
	if f.Evidence.FillDigest != current.FillDigest || f.Evidence.ContractDigest != current.ContractDigest {
		return fmt.Errorf("the evidence of fill %q is stale: its files or its slot contract changed after its tests passed; run stego test again", name)
	}
	digest, err := fill.EvidenceDigest(f)
	if err != nil {
		return err
	}
	key, err := personalSigningKey(input.ProjectDir)
	if err != nil {
		return err
	}
	signature, err := key.sign(input.ProjectDir, digest)
	if err != nil {
		return err
	}
	signers, err := fill.ReadSigners(input.ProjectDir)
	if err != nil {
		return err
	}
	signature.Signer = fill.Principal(signature, digest, signers)
	listed := signature.Signer != ""
	if !listed {
		signature.Signer = key.key
	}
	signOff := fill.FillSignOff{EvidenceDigest: digest, Signature: signature, SignedAt: time.Now().UTC().Truncate(time.Second)}
	if err := fill.WriteSignOff(manifest, signOff); err != nil {
		return err
	}
	event := fill.FillSignedOff{FillName: name, Signer: signature.Signer, EvidenceDigest: digest}
	out, err := yaml.Marshal(map[string]fill.FillSignOff{"sign_off": signOff})
	if err != nil {
		return err
	}
	fmt.Printf("Signed the evidence %s of fill %q as %s.\n%s", event.EvidenceDigest, event.FillName, event.Signer, out)
	if !listed {
		fmt.Printf("SIGNERS does not list this key, so the fill is not qualified until SIGNERS lists it.\n")
	}
	return nil
}

// signingKey is the key that the person's git configuration names for
// signing: its format (ssh or openpgp), the key, and the program that signs.
type signingKey struct {
	format  string
	key     string
	program string
}

// personalSigningKey reads the person's signing key from git config:
// gpg.format and user.signingkey, with gpg.ssh.program or gpg.program. It
// refuses when no signing key is set up, and makes none.
func personalSigningKey(projectDir string) (signingKey, error) {
	config := func(name string) string {
		cmd := exec.Command("git", "config", "--get", name)
		cmd.Dir = projectDir
		out, err := cmd.Output()
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	key := signingKey{format: config("gpg.format"), key: config("user.signingkey")}
	if key.format == "" {
		key.format = "openpgp"
	}
	if key.key == "" {
		return signingKey{}, errors.New("no signing key is set up: set user.signingkey (and gpg.format ssh for an SSH key) in your git config; STEGO never makes or uses a key of its own")
	}
	switch key.format {
	case "ssh":
		key.program = config("gpg.ssh.program")
		if key.program == "" {
			key.program = "ssh-keygen"
		}
	case "openpgp":
		key.program = config("gpg.program")
		if key.program == "" {
			key.program = "gpg"
		}
	default:
		return signingKey{}, fmt.Errorf("gpg.format %q is not supported: use ssh or openpgp", key.format)
	}
	return key, nil
}

// sign signs the digest with the person's key through ssh-keygen or gpg.
func (k signingKey) sign(projectDir, digest string) (fill.Signature, error) {
	var cmd *exec.Cmd
	var keyType string
	switch k.format {
	case "ssh":
		keyType = fill.KeySSH
		keyFile := k.key
		if strings.HasPrefix(keyFile, "key::") {
			return fill.Signature{}, errors.New("user.signingkey names a literal key (key::); name the key file instead, so ssh-keygen signs with it")
		}
		if strings.HasPrefix(keyFile, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				keyFile = filepath.Join(home, keyFile[2:])
			}
		}
		cmd = exec.Command(k.program, "-Y", "sign", "-n", fill.SSHNamespace, "-f", keyFile)
	default:
		keyType = fill.KeyGPG
		cmd = exec.Command(k.program, "--batch", "--yes", "--detach-sign", "--armor", "--local-user", k.key)
	}
	cmd.Dir = projectDir
	cmd.Stdin = strings.NewReader(digest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fill.Signature{}, fmt.Errorf("signing with %s: %v: %s", k.program, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return fill.Signature{}, fmt.Errorf("signing with %s gave no signature", k.program)
	}
	return fill.Signature{KeyType: keyType, Armored: stdout.String()}, nil
}
