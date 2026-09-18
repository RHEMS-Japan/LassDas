package localrun

import (
	"path/filepath"
	"sort"
)

// Spec is everything needed to run one instance, in a form that says nothing
// about where it runs.
//
// The engine used to know one place: a docker daemon on the machine that ran
// setup. Which meant an instance lived on one person's laptop, and every
// delivery for that project stopped when that laptop did. Naming the hosts
// the engine supports - docker here, Kubernetes there - would only move the
// limit: the next host is always the one that is not on the list. So the
// engine describes what has to run, and whoever is doing the installation
// puts it where the requester asked for.
//
// Secret values are deliberately absent. EnvFile names the file that holds
// them, so the installation reads it once and puts it wherever that host
// keeps secrets.
type Spec struct {
	// Name is what the instance should be called wherever it lands.
	Name string `json:"name"`
	// Image is the pinned digest. A tag is never enough to say what ran.
	Image string `json:"image"`
	// EngineSHA is the source commit the image was built from.
	EngineSHA string `json:"engine_sha"`
	// User is the uid:gid the process must run as; the image's own files
	// are owned by it.
	User string `json:"user"`
	// Platform is what the image was built for.
	Platform string `json:"platform"`
	// BoardPort is the port inside the instance that serves the board.
	BoardPort int `json:"board_port"`
	// EnvFile holds the environment, one KEY=value per line, secrets
	// included. Its values are not reproduced here.
	EnvFile string `json:"env_file"`
	// EnvKeys are the names in that file, so an installation can check it
	// has them all without reading the values.
	EnvKeys []string `json:"env_keys"`
	// ConfigDir holds the two configuration files, which are read-only to
	// the process and identical wherever it runs.
	ConfigDir   string   `json:"config_dir"`
	ConfigFiles []string `json:"config_files"`
	// ConfigMountPath is where those files have to appear.
	ConfigMountPath string `json:"config_mount_path"`
	// DataPath is the one writable place the instance needs. It must
	// survive a restart: the ledger, the runs and their records live here.
	DataPath string `json:"data_path"`
	// DataName is the volume name the docker host uses, offered as a
	// default name for whatever the target host calls persistent storage.
	DataName string `json:"data_name"`
	// BeforeMoving is what has to change when this instance leaves the
	// machine it was set up on. Each line is a thing that, carried over as
	// it stands, makes the moved instance wrong quietly - it starts, it
	// looks healthy, and it is not doing what it looks like it is doing.
	BeforeMoving []string `json:"before_moving"`
}

// beforeMoving reads the instance's own environment for the settings that
// only hold on the machine the wizard set up.
func beforeMoving(env map[string]string) []string {
	moving := []string{
		"先にこのマシンの本体を止める。同じ課題管理の project を 2 つの本体が見ると、同じ依頼を二重に処理する",
		"data_path の中身を移す (作り直さない)。台帳は SQLite の WAL なので、止めてから写す。動いたまま写すと壊れた台帳が届く",
	}
	if env["LASSDAS_BOARD_AUTH"] == "local" {
		moving = append(moving,
			"板の認証を変える。いまは local で、loopback の Host にしか答えない (Service や Ingress 越しは 403)。"+
				"移した先では LASSDAS_BOARD_AUTH=basic と LASSDAS_BOARD_USER・LASSDAS_BOARD_PASS (16 文字以上) を入れる")
	}
	return moving
}

// Describe reads an instance and says what has to run, without saying where.
func Describe(i Instance) (Spec, error) {
	p, err := prepare(i)
	if err != nil {
		return Spec{}, err
	}
	keys := make([]string, 0, len(p.env))
	for key := range p.env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	name := resourceName(p.instance)
	return Spec{
		Name:            name,
		Image:           p.instance.Image,
		EngineSHA:       p.instance.EngineSHA,
		User:            "1000:1000",
		Platform:        "linux/arm64",
		BoardPort:       9200,
		EnvFile:         filepath.Join(p.instance.Dir, "runtime.env"),
		EnvKeys:         keys,
		ConfigDir:       filepath.Join(p.instance.Dir, "config"),
		ConfigFiles:     []string{"runtime.json", "m1-consumer.json"},
		ConfigMountPath: "/etc/lassdas/config",
		DataPath:        "/data",
		DataName:        name + "-data",
		BeforeMoving:    beforeMoving(p.env),
	}, nil
}
