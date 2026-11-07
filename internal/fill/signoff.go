package fill

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The kinds of key that may sign a fill's evidence.
const (
	KeySSH = "ssh"
	KeyGPG = "gpg"
)

// SSHNamespace is the namespace of an SSH signature of fill evidence, so a
// signature made for another purpose, such as a commit, does not count.
const SSHNamespace = "stego-fill"

// SignersFile is the file at the project root that lists the keys whose
// signatures may qualify a fill (ST-EXT-TM04).
const SignersFile = "SIGNERS"

// Signature is a person's signature of one fill's evidence (ST-VO44): the
// kind of key, the identity that SIGNERS lists for the key, and the armored
// signature over the evidence digest.
type Signature struct {
	KeyType string `yaml:"key_type"`
	Signer  string `yaml:"signer"`
	Armored string `yaml:"signature"`
}

// armorLines are the first and the last line of the armored signature of
// each key type, as ssh-keygen -Y sign and gpg --detach-sign --armor write it.
var armorLines = map[string][2]string{
	KeySSH: {"-----BEGIN SSH SIGNATURE-----", "-----END SSH SIGNATURE-----"},
	KeyGPG: {"-----BEGIN PGP SIGNATURE-----", "-----END PGP SIGNATURE-----"},
}

// check refuses a signature whose key type is not ssh or gpg, or whose
// signature is not one whole armored detached signature of that key type
// (ST-VO44-C01). The fault names the field.
func (s Signature) check() error {
	lines, ok := armorLines[s.KeyType]
	if !ok {
		return fmt.Errorf("signature.key_type %q is not %s or %s", s.KeyType, KeySSH, KeyGPG)
	}
	text := strings.Split(strings.TrimSpace(strings.ReplaceAll(s.Armored, "\r\n", "\n")), "\n")
	if len(text) < 3 || strings.TrimSpace(text[0]) != lines[0] || strings.TrimSpace(text[len(text)-1]) != lines[1] {
		return fmt.Errorf("signature.signature is not an armored detached %s signature from %s to %s", s.KeyType, lines[0], lines[1])
	}
	for _, line := range text[1 : len(text)-1] {
		if strings.HasPrefix(strings.TrimSpace(line), "-----") {
			return fmt.Errorf("signature.signature holds more than one armored block")
		}
	}
	return nil
}

// The reasons that VerifySignOff refuses a sign-off.
var (
	ErrOtherEvidence = errors.New("the sign-off signs other evidence")
	ErrNotCovered    = errors.New("the signature does not cover the evidence")
	ErrUnlistedKey   = errors.New("SIGNERS does not list the key")
)

// Signers is the SIGNERS file as STEGO reads it. Each SSH key is a line in
// the SSH allowed signers format. Each GPG key is a line that holds the
// identity and the word gpg, followed by the key's armored public key block.
// Comment lines start with #.
type Signers struct {
	sshLines []string
	openPGP  []openPGPSigner
}

// openPGPSigner is one GPG key that SIGNERS lists.
type openPGPSigner struct {
	principal   string
	key         []byte
	fingerprint string
}

// ParseSigners reads the content of a SIGNERS file.
func ParseSigners(data []byte) (Signers, error) {
	var signers Signers
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		fields := strings.Fields(text)
		if len(fields) != 2 || fields[1] != KeyGPG {
			signers.sshLines = append(signers.sshLines, scanner.Text())
			continue
		}
		start := line
		var block []string
		for scanner.Scan() {
			line++
			block = append(block, scanner.Text())
			if strings.TrimSpace(scanner.Text()) == "-----END PGP PUBLIC KEY BLOCK-----" {
				break
			}
		}
		key, err := dearmor(block)
		if err != nil {
			return Signers{}, fmt.Errorf("SIGNERS line %d: the GPG key of %s: %w", start, fields[0], err)
		}
		fingerprint, err := primaryFingerprint(key)
		if err != nil {
			return Signers{}, fmt.Errorf("SIGNERS line %d: the GPG key of %s: %w", start, fields[0], err)
		}
		signers.openPGP = append(signers.openPGP, openPGPSigner{principal: fields[0], key: key, fingerprint: fingerprint})
	}
	if err := scanner.Err(); err != nil {
		return Signers{}, err
	}
	return signers, nil
}

// ReadSigners reads the SIGNERS file at the project root. A project with no
// SIGNERS file lists no key.
func ReadSigners(projectDir string) (Signers, error) {
	data, err := os.ReadFile(filepath.Join(projectDir, SignersFile))
	if errors.Is(err, os.ErrNotExist) {
		return Signers{}, nil
	}
	if err != nil {
		return Signers{}, err
	}
	return ParseSigners(data)
}

// VerifySignOff verifies a sign-off (ST-EXT-DS02). It accepts the sign-off
// only when it names the digest of the fill's current evidence and its
// signature verifies over exactly that digest, by the verification of SSH or
// GPG itself, by a key that SIGNERS lists. It gives the identity that SIGNERS
// lists for the key. A refusal wraps ErrOtherEvidence, ErrNotCovered or
// ErrUnlistedKey.
func VerifySignOff(signOff *FillSignOff, evidenceDigest string, signers Signers) (string, error) {
	if signOff == nil {
		return "", errors.New("the fill has no sign-off")
	}
	if signOff.EvidenceDigest != evidenceDigest {
		return "", ErrOtherEvidence
	}
	switch signOff.Signature.KeyType {
	case KeySSH:
		return verifySSH(signOff.Signature.Armored, evidenceDigest, signers)
	case KeyGPG:
		return verifyOpenPGP(signOff.Signature.Armored, evidenceDigest, signers)
	default:
		return "", fmt.Errorf("%w: the key type %q is not %s or %s", ErrNotCovered, signOff.Signature.KeyType, KeySSH, KeyGPG)
	}
}

// verifySSH verifies an SSH signature with ssh-keygen -Y, against the SSH
// keys of SIGNERS, which ssh-keygen reads in the allowed signers format with
// its options.
func verifySSH(signature, digest string, signers Signers) (string, error) {
	dir, err := os.MkdirTemp("", "stego-verify-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	allowed := filepath.Join(dir, "allowed_signers")
	signatureFile := filepath.Join(dir, "signature")
	if err := os.WriteFile(allowed, []byte(strings.Join(signers.sshLines, "\n")+"\n"), 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(signatureFile, []byte(signature), 0600); err != nil {
		return "", err
	}
	find := exec.Command("ssh-keygen", "-Y", "find-principals", "-s", signatureFile, "-f", allowed)
	output, err := find.Output()
	principals := strings.Fields(string(output))
	if err != nil || len(principals) == 0 {
		return "", ErrUnlistedKey
	}
	var failures []string
	for _, principal := range principals {
		verify := exec.Command("ssh-keygen", "-Y", "verify", "-f", allowed, "-I", principal, "-n", SSHNamespace, "-s", signatureFile)
		verify.Stdin = strings.NewReader(digest)
		if output, err := verify.CombinedOutput(); err == nil {
			return principal, nil
		} else {
			failures = append(failures, strings.TrimSpace(string(output)))
		}
	}
	return "", fmt.Errorf("%w: %s", ErrNotCovered, strings.Join(failures, "; "))
}

// verifyOpenPGP verifies a GPG signature with gpgv, against a keyring that
// holds only the GPG keys of SIGNERS, so a key in the person's own keyring
// counts only when SIGNERS lists it.
func verifyOpenPGP(signature, digest string, signers Signers) (string, error) {
	if len(signers.openPGP) == 0 {
		return "", ErrUnlistedKey
	}
	dir, err := os.MkdirTemp("", "stego-verify-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	var keyring []byte
	for _, signer := range signers.openPGP {
		keyring = append(keyring, signer.key...)
	}
	keyringFile := filepath.Join(dir, "signers.gpg")
	signatureFile := filepath.Join(dir, "signature.asc")
	if err := os.WriteFile(keyringFile, keyring, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(signatureFile, []byte(signature), 0600); err != nil {
		return "", err
	}
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		return "", err
	}
	verify := exec.Command("gpgv", "--homedir", home, "--status-fd", "1", "--keyring", keyringFile, signatureFile, "-")
	verify.Stdin = strings.NewReader(digest)
	output, runErr := verify.Output()
	var valid, unlisted, bad bool
	var fingerprint string
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "[GNUPG:]" {
			continue
		}
		switch fields[1] {
		case "VALIDSIG":
			valid = true
			fingerprint = fields[len(fields)-1]
		case "NO_PUBKEY":
			unlisted = true
		case "ERRSIG":
			if len(fields) >= 8 && fields[7] == "9" {
				unlisted = true
			}
		case "BADSIG":
			bad = true
		}
	}
	if runErr == nil && valid && !bad {
		for _, signer := range signers.openPGP {
			if strings.EqualFold(signer.fingerprint, fingerprint) {
				return signer.principal, nil
			}
		}
		return "", ErrUnlistedKey
	}
	if unlisted {
		return "", ErrUnlistedKey
	}
	return "", ErrNotCovered
}

// Principal gives the identity that SIGNERS lists for the key that made a
// signature over digest, or "" when SIGNERS does not list the key.
func Principal(signature Signature, digest string, signers Signers) string {
	var principal string
	var err error
	switch signature.KeyType {
	case KeySSH:
		principal, err = verifySSH(signature.Armored, digest, signers)
	case KeyGPG:
		principal, err = verifyOpenPGP(signature.Armored, digest, signers)
	}
	if err != nil {
		return ""
	}
	return principal
}

// dearmor decodes an armored OpenPGP public key block into its packets.
func dearmor(lines []string) ([]byte, error) {
	var body strings.Builder
	state := 0
	for _, line := range lines {
		text := strings.TrimSpace(line)
		switch state {
		case 0:
			if text == "-----BEGIN PGP PUBLIC KEY BLOCK-----" {
				state = 1
			}
		case 1:
			if text == "" {
				state = 2
			} else if !strings.Contains(text, ":") {
				state = 2
				body.WriteString(text)
			}
		case 2:
			if strings.HasPrefix(text, "-----END") {
				state = 3
				break
			}
			if strings.HasPrefix(text, "=") {
				continue
			}
			body.WriteString(text)
		}
	}
	if state != 3 {
		return nil, errors.New("the armored public key block is not complete")
	}
	key, err := base64.StdEncoding.DecodeString(body.String())
	if err != nil {
		return nil, fmt.Errorf("the armored public key block: %w", err)
	}
	return key, nil
}

// primaryFingerprint gives the version 4 fingerprint of the first public key
// packet of an OpenPGP key: the SHA-1 digest of the packet's body with its
// fixed header, as RFC 4880 section 12.2 states it.
func primaryFingerprint(key []byte) (string, error) {
	if len(key) < 2 || key[0]&0x80 == 0 {
		return "", errors.New("the key does not start with an OpenPGP packet")
	}
	var tag int
	var length, offset int
	if key[0]&0x40 != 0 {
		tag = int(key[0] & 0x3f)
		switch first := int(key[1]); {
		case first < 192:
			length, offset = first, 2
		case first < 224:
			if len(key) < 3 {
				return "", errors.New("the key packet is cut short")
			}
			length, offset = (first-192)<<8+int(key[2])+192, 3
		case first == 255:
			if len(key) < 6 {
				return "", errors.New("the key packet is cut short")
			}
			length, offset = int(key[2])<<24|int(key[3])<<16|int(key[4])<<8|int(key[5]), 6
		default:
			return "", errors.New("the key packet has a partial length")
		}
	} else {
		tag = int(key[0]>>2) & 0x0f
		switch key[0] & 0x03 {
		case 0:
			length, offset = int(key[1]), 2
		case 1:
			if len(key) < 3 {
				return "", errors.New("the key packet is cut short")
			}
			length, offset = int(key[1])<<8|int(key[2]), 3
		case 2:
			if len(key) < 5 {
				return "", errors.New("the key packet is cut short")
			}
			length, offset = int(key[1])<<24|int(key[2])<<16|int(key[3])<<8|int(key[4]), 5
		default:
			return "", errors.New("the key packet has an indeterminate length")
		}
	}
	if tag != 6 {
		return "", fmt.Errorf("the first packet has the tag %d, not a public key", tag)
	}
	if offset+length > len(key) || length < 1 {
		return "", errors.New("the key packet is cut short")
	}
	body := key[offset : offset+length]
	if body[0] != 4 {
		return "", fmt.Errorf("the key has the version %d; STEGO reads version 4 keys", body[0])
	}
	hash := sha1.New()
	hash.Write([]byte{0x99, byte(length >> 8), byte(length)})
	hash.Write(body)
	return strings.ToUpper(hex.EncodeToString(hash.Sum(nil))), nil
}
