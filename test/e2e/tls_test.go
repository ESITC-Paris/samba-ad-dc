package e2e

// The operator-supplied-TLS row of the B.5 matrix: a domain controller that
// serves LDAPS with a certificate the operator brought, instead of the
// self-signed one samba generates for itself.
//
// Every test function name here is a stable traceability ID (see the
// package comment in main_test.go) and MUST NOT be renamed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/esitc-paris/samba-ad-dc/test/e2e/harness"
)

// Container names. Fixed rather than generated, and within the 15-character
// NetBIOS limit the harness enforces (deployment guide §1.7).
const (
	customTLSDC      = "customtls-dc1"
	customTLSBadKey  = "customtls-bad"
	customTLSPartial = "customtls-two"
	customTLSBadMode = "customtls-mode"
	customTLSBack    = "customtls-back"
)

// Where the material is mounted inside the DC, and what the three variables
// are pointed at.
//
// `/run` is a tmpfs in the constrained profile, and a bind mounted *under* a
// tmpfs works — `/run/secrets/<name>`, which every secret in this suite
// already uses, is the same arrangement. It is deliberately not `/etc/samba`
// or `/var/lib/samba`: those are volumes the DC owns, and material the
// operator renews from outside has no business living on one.
const containerTLSDir = "/run/secrets/tls"

// The distinguished name of the CA this test issues from. It has to be
// recognisable in `openssl x509 -issuer` output and impossible to confuse
// with the CA samba generates for itself (`CN=Samba Administration`).
const customCAName = "samba-ad-dc E2E operator CA"

// customTLSWorstCase is what this test can consume if every budget is spent:
// one provision, two containers that are expected to refuse, and the stop in
// between. Checked before anything is started, for the reason requireDeadline
// documents: being killed by `go test` skips every teardown.
const customTLSWorstCase = harness.HealthTimeout + harness.HealthTransitionTimeout +
	3*harness.ExitTimeout + 2*stopTimeout + 2*time.Minute

// tlsMaterial is the PEM material one run of generateTLSMaterial produced.
type tlsMaterial struct {
	caPEM   string // the CA that signed cert
	certPEM string // a server certificate for the DC's FQDN
	keyPEM  string // its private key
	otherCA string // an unrelated CA, for the negative control
	hostDir string // host directory holding ca.pem, cert.pem and key.pem
}

// materialModes is how the three files are laid out for the DC, and it is
// samba's rule rather than a preference: the private key must be EXACTLY 0600
// and owned by the user samba runs as, or samba refuses to start its LDAP
// server at all (it cites CVE-2013-4476 and terminates). The certificate and
// the CA are public and samba reads them at any mode.
var materialModes = map[string]os.FileMode{
	"ca.pem":   0o644,
	"cert.pem": 0o644,
	"key.pem":  0o600,
}

// pemMarker prefixes each file in the generator's output. The PEM blocks are
// copied out of the container's stdout rather than through a bind mount
// because the client image gets no mounts in this suite; nothing here is
// secret to anyone but this test, whose material dies with it.
const pemMarker = "-----E2E FILE "

// generateTLSMaterial issues, inside the CLIENT container, a CA and a server
// certificate for the DC's FQDN, plus a second unrelated CA.
//
// The client container is used rather than the host's openssl on purpose:
// it is the one place in this suite whose toolchain is pinned (its base
// image is pinned by digest), so what this test issues is the same on a
// developer laptop and on both CI architectures. A host openssl would be
// whatever that machine happens to carry — LibreSSL on macOS, which does not
// take `-addext` at all.
func generateTLSMaterial(t *testing.T, net, fqdn string) *tlsMaterial {
	t.Helper()

	// One SAN, the DC's FQDN, because that is the name a client dials it by
	// and the only name LDAPS has to verify. `-addext` needs OpenSSL 1.1.1+;
	// the client image carries 3.x.
	script := `set -e
cd /tmp
openssl req -x509 -newkey rsa:2048 -sha256 -days 30 -nodes \
  -keyout ca.key -out ca.pem -subj "/CN=$E2E_CA_NAME" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
openssl req -x509 -newkey rsa:2048 -sha256 -days 30 -nodes \
  -keyout other.key -out otherca.pem -subj "/CN=$E2E_CA_NAME (unrelated)" \
  -addext "basicConstraints=critical,CA:TRUE" \
  -addext "keyUsage=critical,keyCertSign,cRLSign" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout key.pem -out server.csr \
  -subj "/CN=$E2E_FQDN" 2>/dev/null
printf 'subjectAltName=DNS:%s\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n' \
  "$E2E_FQDN" > ext.cnf
openssl x509 -req -in server.csr -CA ca.pem -CAkey ca.key -CAcreateserial \
  -days 30 -sha256 -extfile ext.cnf -out cert.pem 2>/dev/null
for f in ca.pem cert.pem key.pem otherca.pem; do
  echo "` + pemMarker + `$f-----"
  cat "$f"
done`

	out := harness.Client(t, net, map[string]string{
		"E2E_FQDN":    fqdn,
		"E2E_CA_NAME": customCAName,
	}, "sh", "-c", script)

	files := splitMarkedFiles(out)
	m := &tlsMaterial{
		caPEM:   files["ca.pem"],
		certPEM: files["cert.pem"],
		keyPEM:  files["key.pem"],
		otherCA: files["otherca.pem"],
	}
	for name, content := range map[string]string{
		"ca.pem": m.caPEM, "cert.pem": m.certPEM, "key.pem": m.keyPEM, "otherca.pem": m.otherCA,
	} {
		if !strings.Contains(content, "-----BEGIN") {
			t.Fatalf("openssl did not produce %s:\n%s", name, out)
		}
	}

	m.hostDir = t.TempDir()
	for name, content := range map[string]string{
		"ca.pem": m.caPEM, "cert.pem": m.certPEM, "key.pem": m.keyPEM,
	} {
		p := filepath.Join(m.hostDir, name)
		if err := os.WriteFile(p, []byte(content), materialModes[name]); err != nil { //nolint:gosec // throwaway test material; the modes are samba's, see materialModes
			t.Fatalf("writing %s: %v", p, err)
		}
	}
	// The modes again, and the ownership — neither of which os.WriteFile can
	// be trusted with here: its mode is masked by the umask, and a test
	// process cannot create a root-owned file at all.
	harness.OwnAsRoot(t, m.hostDir, materialModes)
	return m
}

// splitMarkedFiles turns the generator's marked stdout back into files.
func splitMarkedFiles(out string) map[string]string {
	files := map[string]string{}
	current := ""
	var b strings.Builder
	flush := func() {
		if current != "" {
			files[current] = b.String()
		}
		b.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), pemMarker); ok {
			flush()
			current = strings.TrimSuffix(name, "-----")
			continue
		}
		if current != "" {
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	flush()
	return files
}

// tlsEnv is the environment for the three SAMBA_TLS_* variables.
func tlsEnv(cert, key, ca string) map[string]string {
	return map[string]string{
		"SAMBA_TLS_CERT_FILE": cert,
		"SAMBA_TLS_KEY_FILE":  key,
		"SAMBA_TLS_CA_FILE":   ca,
	}
}

// TestCustomTLSMaterial covers B.5 row N10: a DC provisioned with
// `SAMBA_TLS_CERT_FILE` / `SAMBA_TLS_KEY_FILE` / `SAMBA_TLS_CA_FILE` serves
// LDAPS with the operator's certificate, and a trio that is incomplete or
// points at a file that is not there refuses the boot.
//
// Five things are asserted, and none of them alone would be enough:
//
//   - samba's own parser reads the three paths back out of the smb.conf on
//     the configuration volume — the image applied them;
//   - the certificate presented on :636 is issued by OUR CA and carries the
//     DC's FQDN — samba is actually serving them, not merely configured with
//     them. An image that ignored the variables would present the
//     self-signed certificate it generates for itself and fail here;
//   - a real LDAPS client verifying against our CA and nothing else gets an
//     answer, and the same client verifying against a CA that did not sign
//     it does not — the chain is genuinely being checked;
//   - a path that is not mounted exits 11 naming the variable, and a trio
//     missing one variable exits 10 naming the one to set;
//   - unsetting the three variables is a way BACK: the three settings are
//     taken out of smb.conf again and the DC returns to samba's own material.
//     Without that the trio would be a one-way door, since smb.conf lives on
//     a volume and those keys are exactly the ones SAMBA_GLOBAL_OPTIONS
//     refuses;
//   - a private key at any mode but 0600 exits 11 before any daemon starts.
//     That last one is not this image's rule but samba's, it is exact (0400
//     is refused as surely as 0644) and it is fatal — samba refuses to start
//     its LDAP server and terminates the DC, citing CVE-2013-4476. Measured;
//     see the profile's *Bring your own TLS material*.
func TestCustomTLSMaterial(t *testing.T) {
	requireDeadline(t, customTLSWorstCase)
	net := harness.Network(t)
	fqdn := harness.FQDN(customTLSDC)

	material := generateTLSMaterial(t, net, fqdn)

	cert := containerTLSDir + "/cert.pem"
	key := containerTLSDir + "/key.pem"
	ca := containerTLSDir + "/ca.pem"

	env := tlsEnv(cert, key, ca)
	env["SAMBA_REALM"] = harness.Realm
	env["SAMBA_DOMAIN"] = harness.Domain

	dc := harness.StartDC(t, net, customTLSDC, "provision", env,
		harness.AdminSecret(t),
		harness.WithBind(material.hostDir, containerTLSDir, true))
	harness.WaitHealthy(t, dc.Name, harness.HealthTimeout)

	// --- the image applied them -----------------------------------------
	for _, want := range []struct{ key, value string }{
		{"tls certfile", cert},
		{"tls keyfile", key},
		{"tls cafile", ca},
	} {
		if got := globalSetting(t, dc.Name, want.key); got != want.value {
			t.Fatalf("samba reads %q = %q back from %s, want %q\n--- logs ---\n%s",
				want.key, got, smbConfPath, want.value, harness.Logs(t, dc.Name))
		}
	}

	// --- samba is serving them ------------------------------------------
	//
	// The address is dialled rather than the name, with -servername carrying
	// the name, so this half cannot pass because DNS happened to resolve
	// somewhere else.
	inspect := "openssl s_client -connect " + dc.IP + ":636 -servername " + fqdn +
		" </dev/null 2>/dev/null | openssl x509 -noout -issuer -subject -ext subjectAltName"
	presented := harness.Client(t, net, map[string]string{harness.ClientDNSEnv: dc.IP}, "sh", "-c", inspect)
	// The exact spelling openssl 3.x in the pinned client image prints
	// (measured): `issuer=CN=<name>`, no spaces around the equals signs.
	mustContain(t, "certificate presented on "+dc.IP+":636", presented,
		"issuer=CN="+customCAName,
		"subject=CN="+fqdn,
		"DNS:"+fqdn)
	// Samba's own CA must not be what is on the wire. Asserted by name, so a
	// future image that silently fell back to self-signed material is caught
	// here rather than in the (weaker) negative control below.
	if strings.Contains(presented, "Samba Administration") {
		t.Fatalf("the DC is serving the certificate samba generated for itself, not the operator's:\n%s", presented)
	}

	// --- a real client verifies the chain --------------------------------
	verifyEnv := map[string]string{
		harness.ClientDNSEnv: dc.IP,
		"E2E_CA_PEM":         material.caPEM,
		"E2E_OTHER_CA_PEM":   material.otherCA,
	}
	const writeCAs = `printf %s "$E2E_CA_PEM" > /tmp/ca.pem; printf %s "$E2E_OTHER_CA_PEM" > /tmp/other.pem; `
	search := "ldapsearch -H ldaps://" + fqdn + ":636 -x -s base -b '' " +
		"defaultNamingContext dnsHostName"

	out := harness.Client(t, net, verifyEnv, "sh", "-c",
		writeCAs+"LDAPTLS_CACERT=/tmp/ca.pem LDAPTLS_REQCERT=demand "+search)
	mustContain(t, "ldaps rootDSE with the operator's CA", out,
		"defaultNamingContext: "+harness.BaseDN(),
		"dnsHostName: "+fqdn,
		"result: 0 Success")

	code, out := harness.ClientErr(t, net, verifyEnv, "sh", "-c",
		writeCAs+"LDAPTLS_CACERT=/tmp/other.pem LDAPTLS_REQCERT=demand "+search)
	if code == 0 {
		t.Fatalf("ldaps succeeded against a CA that did not sign the DC's certificate; "+
			"the client is not verifying the chain:\n%s", out)
	}

	// The DC's own self-signed CA, when samba still generated one, is the
	// sharpest negative control there is: it is exactly the trust anchor an
	// operator would have used before switching to their own material, and
	// it must now be useless.
	const dcOwnCA = "/var/lib/samba/private/tls/ca.pem"
	if exit, _ := harness.ExecErr(t, dc.Name, "sh", "-c", "test -s "+dcOwnCA); exit == 0 {
		own := harness.CopyFrom(t, dc.Name, dcOwnCA)
		code, out := harness.ClientErr(t, net, map[string]string{
			harness.ClientDNSEnv: dc.IP,
			"E2E_CA_PEM":         string(own),
		}, "sh", "-c", `printf %s "$E2E_CA_PEM" > /tmp/ca.pem; `+
			"LDAPTLS_CACERT=/tmp/ca.pem LDAPTLS_REQCERT=demand "+search)
		if code == 0 {
			t.Fatalf("ldaps succeeded against the DC's OWN generated CA: the operator's "+
				"material is not what is being served:\n%s", out)
		}
	} else {
		t.Logf("samba generated no %s on this provision, so the sharpest negative control "+
			"is not available; the unrelated-CA control above and the issuer assertion carry "+
			"the property", dcOwnCA)
	}

	if code := harness.Stop(t, dc.Name, stopTimeout); code != 0 {
		t.Fatalf("`docker stop` left container %s with exit code %d, want 0\n--- logs ---\n%s",
			dc.Name, code, harness.Logs(t, dc.Name))
	}

	// --- a path that is not mounted -------------------------------------
	//
	// On the SAME volumes, in run mode: the check belongs to every start, not
	// only to the boot that provisioned. It is exit 11 — the secret class —
	// because one of the three files is a private key.
	missing := containerTLSDir + "/not-mounted.pem"
	badEnv := tlsEnv(cert, missing, ca)
	exit, logs := harness.RunDCExpectExit(t, net, customTLSBadKey, "run", badEnv,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume),
		harness.WithBind(material.hostDir, containerTLSDir, true))
	if exit != exitSecretError {
		t.Fatalf("a SAMBA_TLS_KEY_FILE that is not mounted exited %d, want %d (secret error)\n--- logs ---\n%s",
			exit, exitSecretError, logs)
	}
	mustContain(t, "refusal of "+customTLSBadKey, logs, "SAMBA_TLS_KEY_FILE", missing)
	if strings.Contains(logs, "BEGIN PRIVATE KEY") {
		t.Fatalf("the refusal quoted key material:\n%s", logs)
	}

	// --- two of the three ------------------------------------------------
	partial := map[string]string{"SAMBA_TLS_CERT_FILE": cert, "SAMBA_TLS_KEY_FILE": key}
	exit, logs = harness.RunDCExpectExit(t, net, customTLSPartial, "run", partial,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume),
		harness.WithBind(material.hostDir, containerTLSDir, true))
	if exit != exitConfigError {
		t.Fatalf("a trio missing SAMBA_TLS_CA_FILE exited %d, want %d (configuration error)\n--- logs ---\n%s",
			exit, exitConfigError, logs)
	}
	mustContain(t, "refusal of "+customTLSPartial, logs, "SAMBA_TLS_CA_FILE")

	// --- a key at the wrong mode -----------------------------------------
	//
	// The likeliest mistake of the three, and the one whose unguarded
	// failure is worst: samba does not warn about a 0644 private key, it
	// refuses to start the LDAP server and terminates the whole DC, twenty
	// lines deep in its own output. Done last, because it modifies the
	// material every container above shares.
	harness.OwnAsRoot(t, material.hostDir, map[string]os.FileMode{"key.pem": 0o644})
	exit, logs = harness.RunDCExpectExit(t, net, customTLSBadMode, "run", tlsEnv(cert, key, ca),
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume),
		harness.WithBind(material.hostDir, containerTLSDir, true))
	if exit != exitSecretError {
		t.Fatalf("a world-readable SAMBA_TLS_KEY_FILE exited %d, want %d (secret error)\n--- logs ---\n%s",
			exit, exitSecretError, logs)
	}
	mustContain(t, "refusal of "+customTLSBadMode, logs, "SAMBA_TLS_KEY_FILE", "0600")

	// --- the way back ----------------------------------------------------
	//
	// A new container on the SAME volumes with the three variables gone and
	// the material not even mounted — an operator undoing the change. The
	// three settings must be taken back OUT of smb.conf: they live on a
	// volume, and SAMBA_GLOBAL_OPTIONS refuses those very keys, so a DC that
	// kept them would point at a mount that no longer exists with no remedy
	// at all.
	//
	// What samba then does was measured rather than assumed: it autogenerates
	// its self-signed material on demand at start ("Attempting to
	// autogenerate TLS self-signed keys … TLS self-signed keys generated
	// OK"), even on a DC that never had any, and LDAPS comes back up on it.
	back := harness.StartDC(t, net, customTLSBack, "run", nil,
		harness.WithVolumes(dc.StateVolume, dc.ConfVolume))
	harness.WaitHealthy(t, back.Name, harness.HealthTransitionTimeout)

	backLogs := harness.Logs(t, back.Name)
	for _, key := range []string{"tls certfile", "tls keyfile", "tls cafile"} {
		mustContain(t, "the way back on "+back.Name, backLogs,
			`removed "`+key+`" from `+smbConfPath+": SAMBA_TLS_*_FILE are unset")
	}
	// Read back through samba's own parser, which is what decides: with the
	// lines gone it reports the compile-time defaults, relative to the
	// private directory on the state volume.
	for _, want := range []struct{ key, value string }{
		{"tls certfile", "tls/cert.pem"},
		{"tls keyfile", "tls/key.pem"},
		{"tls cafile", "tls/ca.pem"},
	} {
		if got := globalSetting(t, back.Name, want.key); got != want.value {
			t.Fatalf("after unsetting the variables samba still reads %q = %q, want its default %q\n--- logs ---\n%s",
				want.key, got, want.value, backLogs)
		}
	}
	// And LDAPS actually works again, on material samba made for itself.
	regenerated := harness.CopyFrom(t, back.Name, "/var/lib/samba/private/tls/ca.pem")
	backSearch := "ldapsearch -H ldaps://" + fqdn + ":636 -x -s base -b '' dnsHostName"
	out = harness.Client(t, net, map[string]string{
		harness.ClientDNSEnv: back.IP,
		"E2E_CA_PEM":         string(regenerated),
	}, "sh", "-c", `printf %s "$E2E_CA_PEM" > /tmp/ca.pem; `+
		"LDAPTLS_CACERT=/tmp/ca.pem LDAPTLS_REQCERT=demand "+backSearch)
	mustContain(t, "ldaps after the way back", out,
		"dnsHostName: "+fqdn, "result: 0 Success")

	// The operator's CA must now be useless — the mirror image of the first
	// half of this test, and what proves the DC really did change back.
	code, out = harness.ClientErr(t, net, map[string]string{
		harness.ClientDNSEnv: back.IP,
		"E2E_CA_PEM":         material.caPEM,
	}, "sh", "-c", `printf %s "$E2E_CA_PEM" > /tmp/ca.pem; `+
		"LDAPTLS_CACERT=/tmp/ca.pem LDAPTLS_REQCERT=demand "+backSearch)
	if code == 0 {
		t.Fatalf("ldaps still verifies against the operator's CA after the variables were "+
			"unset: the material was not taken back out:\n%s", out)
	}
}
