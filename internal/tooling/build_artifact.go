package tooling

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Dunky13/agent-job-queue/internal/backend"
)

var artifactPath = regexp.MustCompile(`^[A-Za-z0-9./_+() -]+$`)

// checkedZIP refuses ambiguous paths and links before any extraction. Resource
// forks remain opaque __MACOSX entries for ditto to restore on macOS.
func checkedZIP(files []*zip.File) (map[string]*zip.File, error) {
	result := make(map[string]*zip.File)
	for _, file := range files {
		name := strings.TrimSuffix(file.Name, "/")
		if !artifactPath.MatchString(file.Name) || name == "" || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") ||
			(!file.Mode().IsRegular() && !file.Mode().IsDir()) {
			return nil, errors.New("Unsafe source artifact archive entry")
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("Duplicate source artifact archive entry")
		}
		result[name] = file
	}
	for name := range result {
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if file, exists := result[parent]; exists && !file.Mode().IsDir() {
				return nil, errors.New("Archive file shadows a directory")
			}
		}
	}
	return result, nil
}

func zipMetadata(file *zip.File) ([]byte, error) {
	if file == nil || !file.Mode().IsRegular() || file.UncompressedSize64 > 64<<10 {
		return nil, errors.New("Missing or oversized source artifact metadata")
	}
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(io.LimitReader(reader, 64<<10+1))
}

func fileDigest(filename string) (string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err = io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// VerifyCheckpointOrigin also binds legacy state, whose metadata predates
// artifact IDs, to the exact original installer contents. Recompression is
// expected; bytes and modes inside the envelope must remain identical.
func VerifyCheckpointOrigin(original, initial string) error {
	left, err := zip.OpenReader(original)
	if err != nil {
		return err
	}
	defer left.Close()
	right, err := zip.OpenReader(initial)
	if err != nil {
		return err
	}
	defer right.Close()
	leftFiles, err := checkedZIP(left.File)
	if err != nil {
		return err
	}
	rightFiles, err := checkedZIP(right.File)
	if err != nil {
		return err
	}
	if len(leftFiles) != len(rightFiles) {
		return errors.New("Checkpoint initial installer differs from the original source artifact")
	}
	for name, source := range leftFiles {
		retained := rightFiles[name]
		if retained == nil || retained.Mode() != source.Mode() || retained.UncompressedSize64 != source.UncompressedSize64 {
			return errors.New("Checkpoint source contents or modes differ")
		}
		if source.Mode().IsDir() {
			continue
		}
		original, err := source.Open()
		if err != nil {
			return err
		}
		checkpoint, err := retained.Open()
		if err != nil {
			original.Close()
			return err
		}
		first, second := sha256.New(), sha256.New()
		_, firstErr := io.Copy(first, original)
		_, secondErr := io.Copy(second, checkpoint)
		err = errors.Join(firstErr, secondErr, original.Close(), checkpoint.Close())
		if err != nil {
			return err
		}
		if !bytes.Equal(first.Sum(nil), second.Sum(nil)) {
			return errors.New("Checkpoint source bytes differ")
		}
	}
	return nil
}

// VerifyBuildArtifact reads only metadata and hashes. It never executes source
// binaries or source scripts. The caller supplies API-verified build identity.
func VerifyBuildArtifact(archive, digest, commit string, build int, output string) (string, error) {
	hash, err := fileDigest(archive)
	if err != nil {
		return "", err
	}
	if digest != "sha256:"+hash {
		return "", errors.New("Source artifact GitHub digest mismatch")
	}
	outer, err := zip.OpenReader(archive)
	if err != nil {
		return "", err
	}
	defer outer.Close()
	files, err := checkedZIP(outer.File)
	if err != nil {
		return "", err
	}
	data, err := zipMetadata(files["agent-job-queue.json"])
	if err != nil {
		return "", err
	}
	manifest, err := backend.ValidateManifest(data)
	if err != nil {
		return "", err
	}
	if manifest.Repository != backend.ReleaseRepository || manifest.Commit == nil || *manifest.Commit != commit || manifest.Build == nil || *manifest.Build != build {
		return "", errors.New("Source artifact manifest repository, commit or build mismatch")
	}
	image := strings.TrimSuffix(manifest.Asset, ".zip") + ".dmg"
	expected := []string{manifest.Asset, manifest.Asset + ".sha256", image, image + ".sha256", "agent-job-queue.json", "agent-job-queue.rb"}
	if len(files) != len(expected) {
		return "", errors.New("Unexpected source artifact contents")
	}
	for _, name := range expected {
		if files[name] == nil || !files[name].Mode().IsRegular() {
			return "", errors.New("Incomplete source artifact")
		}
	}
	// The original installer can be large. Stage only the verified outer regular
	// files; nested signed bundles stay inside their unchanged ZIP until ditto.
	if err = os.Mkdir(output, 0700); err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(output)
		}
	}()
	for _, name := range expected {
		reader, err := files[name].Open()
		if err != nil {
			return "", err
		}
		destination, err := os.OpenFile(filepath.Join(output, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			reader.Close()
			return "", err
		}
		_, copyErr := io.Copy(destination, reader)
		err = errors.Join(copyErr, reader.Close(), destination.Close())
		if err != nil {
			return "", err
		}
	}
	for _, name := range []string{manifest.Asset, image} {
		hash, err := fileDigest(filepath.Join(output, name))
		if err != nil {
			return "", err
		}
		checksum, err := zipMetadata(files[name+".sha256"])
		if err != nil || string(checksum) != hash+"  "+name+"\n" || (name == manifest.Asset && hash != manifest.SHA256) {
			return "", errors.New("Original installer checksum mismatch")
		}
	}
	installer, err := zip.OpenReader(filepath.Join(output, manifest.Asset))
	if err != nil {
		return "", err
	}
	defer installer.Close()
	entries, err := checkedZIP(installer.File)
	if err != nil {
		return "", err
	}
	base := "Agent Job Queue Installer.app/"
	for name := range entries {
		if name != strings.TrimSuffix(base, "/") && !strings.HasPrefix(name, base) && name != "__MACOSX" && !strings.HasPrefix(name, "__MACOSX/") {
			return "", errors.New("Unexpected installer archive root")
		}
	}
	for _, name := range []string{"Contents/Resources/build-info.json", "Contents/Resources/payload/source/build-info.json", "Contents/Resources/payload/Agent Queue.app/Contents/Resources/build-info.json"} {
		data, err := zipMetadata(entries[base+name])
		if err != nil {
			return "", err
		}
		identity, err := backend.ValidateBuildInfo(data)
		if err != nil || !identity.Equal(manifest.BuildInfo) {
			return "", fmt.Errorf("Embedded source build identity mismatch: %s", name)
		}
	}
	version, err := zipMetadata(entries[base+"Contents/Resources/payload/source/VERSION"])
	if err != nil || !bytes.Equal(version, []byte(manifest.Version+"\n")) {
		return "", errors.New("Embedded source version mismatch")
	}
	complete = true
	return manifest.Version, nil
}
