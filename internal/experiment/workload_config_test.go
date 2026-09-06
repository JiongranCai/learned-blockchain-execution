package experiment_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/crypto-org-chain/go-block-stm/internal/experiment"
)

// Include the formal templates: their environment placeholders deliberately
// prevent running, but their workload definitions must still be usable.
func TestWorkloadConfigsGenerate(t *testing.T) {
	root := filepath.Join("..", "..", "configs", "experiments")
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		t.Run(path, func(t *testing.T) {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var config experiment.Config
			decoder := json.NewDecoder(f)
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&config); err != nil {
				t.Fatal(err)
			}
			if config.RunClass != "formal" {
				if _, err := experiment.LoadConfig(path); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := experiment.LoadWorkload(config.Workload); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
