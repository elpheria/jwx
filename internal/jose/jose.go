package jose

import (
	"bytes"
	"context"
	"io/ioutil"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/lestrrat-go/jwx/internal/jwxtest"
	"github.com/lestrrat-go/pdebug"
	"github.com/pkg/errors"
)

var executablePath string
var muExecutablePath sync.RWMutex

func init() {
	findExecutable()
}

func SetExecutable(path string) {
	muExecutablePath.Lock()
	defer muExecutablePath.Unlock()
	executablePath = path
}

func findExecutable() {
	p, err := exec.LookPath("jose")
	if err == nil {
		SetExecutable(p)
	}
}

func ExecutablePath() string {
	muExecutablePath.RLock()
	defer muExecutablePath.RUnlock()

	return executablePath
}

func Available() bool {
	muExecutablePath.RLock()
	defer muExecutablePath.RUnlock()

	return executablePath != ""
}

// GenerateJwk creates a new key using the jose tool, and returns its filename and
// a cleanup function.
// The caller is responsible for calling the cleanup
// function and make sure all resources are released
func GenerateJwk(ctx context.Context, t *testing.T, template string) (string, func(), error) {
	t.Helper()

	file, cleanup, err := jwxtest.CreateTempFile("jwx-jose-key-*.jwk")
	if err != nil {
		return "", nil, errors.Wrap(err, "failed to create temporary file")
	}

	var errdst bytes.Buffer

	cmd := exec.CommandContext(ctx, ExecutablePath(), "jwk", "gen", "-i", template, "-o", file.Name())
	cmd.Stderr = &errdst

	if err := cmd.Run(); err != nil {
		defer cleanup()
		t.Logf(`failed to execute command: %s`, errdst.String())

		return "", nil, errors.Wrap(err, `failed to generate key`)
	}

	return file.Name(), cleanup, nil
}

// EncryptJwe creats an encrypted JWE message and returns its filename and
// a cleanup function.
// The caller is responsible for calling the cleanup
// function and make sure all resources are released
func EncryptJwe(ctx context.Context, t *testing.T, payload []byte, keyfile string) (string, func(), error) {
	t.Helper()

	cmdargs := []string{ExecutablePath(), "jwe", "enc", "-k", keyfile}
	if len(payload) > 0 {
		pfile, pcleanup, perr := jwxtest.CreateTempFile("jwx-jose-payload-*")
		if perr != nil {
			return "", nil, errors.Wrap(perr, `failed to create temporary file`)
		}
		//nolint:errcheck
		pfile.Write(payload)
		//nolint:errcheck
		pfile.Sync()

		cmdargs = append(cmdargs, "-I", pfile.Name())
		defer pcleanup()
	}

	ofile, ocleanup, oerr := jwxtest.CreateTempFile(`jwx-jose-key-*.jwe`)
	if oerr != nil {
		return "", nil, errors.Wrap(oerr, "failed to create temporary file")
	}

	cmdargs = append(cmdargs, "-o", ofile.Name())

	var errdst bytes.Buffer
	t.Logf("Executing `%s`\n", strings.Join(cmdargs, " "))
	cmd := exec.CommandContext(ctx, cmdargs[0], cmdargs[1:]...)
	cmd.Stderr = &errdst

	if err := cmd.Run(); err != nil {
		defer ocleanup()
		t.Logf("Error executing command: %s", errdst.String())

		return "", nil, errors.Wrap(err, `failed to encrypt message`)
	}

	return ofile.Name(), ocleanup, nil
}

func DecryptJwe(ctx context.Context, t *testing.T, cfile, kfile string) ([]byte, error) {
	t.Helper()

	cmdargs := []string{ExecutablePath(), "jwe", "dec", "-i", cfile, "-k", kfile}
	if pdebug.Enabled {
		cbuf, _ := ioutil.ReadFile(cfile)
		pdebug.Printf(`JWE message file contains "%s"`, cbuf)
		kbuf, _ := ioutil.ReadFile(kfile)
		pdebug.Printf(`JWK key file contains "%s"`, kbuf)
	}

	var errdst bytes.Buffer
	var output bytes.Buffer
	t.Logf("Executing `%s`\n", strings.Join(cmdargs, " "))
	cmd := exec.CommandContext(ctx, cmdargs[0], cmdargs[1:]...)
	cmd.Stderr = &errdst
	cmd.Stdout = &output

	if err := cmd.Run(); err != nil {
		t.Logf("Error executing command: %s", err)
		if output.Len() > 0 {
			t.Logf("captured output: %s", output.String())
		}

		if errdst.Len() > 0 {
			t.Logf("captured error: %s", errdst.String())
		}

		jwxtest.DumpFile(t, cfile)
		jwxtest.DumpFile(t, kfile)

		return nil, errors.Wrap(err, `failed to decrypt message`)
	}

	return output.Bytes(), nil
}
