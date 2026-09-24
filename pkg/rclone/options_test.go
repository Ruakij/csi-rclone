package rclone

import "testing"

func TestValidateOptions(t *testing.T) {
	const s3Config = "[store]\ntype = s3\nprovider = Minio\nendpoint = http://minio.example.com:9000\n"
	for _, tc := range []struct {
		name       string
		remote     string
		configData string
		flags      map[string]string
		ok         bool
	}{
		{"on-the-fly s3", "s3", "", map[string]string{"s3-provider": "Minio", "s3-endpoint": "http://minio:9000", "vfs-cache-mode": "full"}, true},
		{"flag spellings", "s3", "", map[string]string{"S3_ACCESS_KEY_ID": "x", "--dir-cache-time": "5s", "umask": "022"}, true},
		{"gcs by prefix", "gcs", "", map[string]string{"gcs-project-number": "1"}, true},
		{"config remote", "store", s3Config, nil, true},
		{"crypt over config remote", "secret", s3Config + "[secret]\ntype = crypt\nremote = store:bucket\npassword = x\n", nil, true},
		{"crypt over on-the-fly", "crypt", "", map[string]string{"crypt-remote": ":s3:bucket", "crypt-password": "x"}, true},

		{"local remote", "local", "", nil, false},
		{"alias remote", "a", "[a]\ntype = alias\nremote = /etc\n", nil, false},
		{"connection string remote", "s3,endpoint=http://169.254.169.254", "", nil, false},
		{"remote with colon", "s3:x", "", nil, false},
		{"unknown section key", "store", s3Config + "env_auth = true\n", nil, false},
		{"section key with dashes", "store", s3Config + "env-auth = true\n", nil, false},
		{"config without section", "store", "type = s3\n", nil, false},
		{"malformed config line", "store", s3Config + "garbage\n", nil, false},
		{"crypt over local path", "c", "[c]\ntype = crypt\nremote = /etc\n", nil, false},
		{"crypt over connection string", "crypt", "", map[string]string{"crypt-remote": ":s3,env_auth=true:bucket"}, false},
		{"crypt over unknown remote", "crypt", "", map[string]string{"crypt-remote": "nope:bucket"}, false},
		{"crypt over crypt", "c1", "[c1]\ntype = crypt\nremote = c2:\n[c2]\ntype = crypt\nremote = c1:\n", nil, false},
		{"env auth flag", "s3", "", map[string]string{"s3-env-auth": "true"}, false},
		{"env auth underscores", "s3", "", map[string]string{"s3_env_auth": "true"}, false},
		{"cache dir", "s3", "", map[string]string{"cache-dir": "/"}, false},
		{"log file", "s3", "", map[string]string{"log-file": "/etc/cron.d/x"}, false},
		{"password command", "s3", "", map[string]string{"password-command": "sh"}, false},
		{"config env remote", "s3", "", map[string]string{"config-x-type": "local"}, false},
		{"sftp ssh", "sftp", "", map[string]string{"sftp-ssh": "sh -c id"}, false},
		{"sftp key file", "sftp", "", map[string]string{"sftp-key-file": "/etc/shadow"}, false},
		{"webdav unix socket", "webdav", "", map[string]string{"webdav-unix-socket": "/run/containerd/containerd.sock"}, false},
		{"fuse option", "s3", "", map[string]string{"option": "allow_root"}, false},
		{"local option", "s3", "", map[string]string{"local-no-check-updated": "true"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateOptions(tc.remote, tc.configData, tc.flags)
			if (err == nil) != tc.ok {
				t.Errorf("validateOptions() = %v, want ok %v", err, tc.ok)
			}
		})
	}
}

func TestValidateOptionsSettings(t *testing.T) {
	t.Cleanup(func() { UnrestrictedOptions, AllowedBackends, AllowedEndpoints = false, nil, nil })

	UnrestrictedOptions = true
	if err := validateOptions("local", "", map[string]string{"log-file": "/x"}); err != nil {
		t.Errorf("unrestricted: %v", err)
	}
	UnrestrictedOptions = false

	AllowedBackends = []string{"gcs"}
	if validateOptions("google cloud storage", "", nil) != nil || validateOptions("s3", "", nil) == nil {
		t.Error("allowed backends not applied")
	}
	AllowedBackends = nil

	AllowedEndpoints = []string{"minio.example.com", ".storage.example.com"}
	for value, ok := range map[string]bool{
		"http://minio.example.com:9000":  true,
		"https://eu.storage.example.com": true,
		"minio.example.com:22":           true,
		"http://169.254.169.254/":        false,
		"http://storage.example.com":     false,
		"evilstorage.example.com":        false,
	} {
		err := validateOptions("s3", "", map[string]string{"s3-endpoint": value})
		if (err == nil) != ok {
			t.Errorf("endpoint %q: %v, want ok %v", value, err, ok)
		}
	}
	if validateOptions("sftp", "", map[string]string{"sftp-host": "10.0.0.1"}) == nil {
		t.Error("sftp host not checked")
	}
	if validateOptions("onedrive", "", map[string]string{"onedrive-tenant-url": "https://10.0.0.1/_api"}) == nil {
		t.Error("onedrive tenant_url not checked")
	}
}
