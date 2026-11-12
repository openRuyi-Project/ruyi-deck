# ruyi-deck

The Operations Deck for openRuyi Repositories and Releases.

## Build

Requires Go 1.23 or later.

```sh
make build
```

The binary is written to `bin/ruyi-deck-linux-<arch>`. Deploy it as `ruyi-deck`
with a `config/` directory beside it.

## Configure

Start with the sample configuration and artifact manifest:

```sh
cp config/config.sample.yaml config/config.yaml
cp config/dailybuild-artifacts.sample.yaml config/dailybuild-artifacts.yaml
```

Replace the example URLs, repository names, and paths for your deployment, and set
`artifact_manifest: dailybuild-artifacts.yaml` in `config.yaml`.
Deployment YAML files in `config/` are ignored by Git; only `*.sample.yaml` files
are intended for publication.

The default configuration is `<executable-directory>/config/config.yaml`.
Use `--config <path>` to override it. Relative paths are resolved beside the
configuration file.

Runtime dependencies:

- Repository operations: DNF 4 with the `reposync` plugin.
- RPM artifacts: `rpm2cpio` and GNU `cpio`.

For GitHub artifacts, provide a token with repository Actions read access through
the environment variable named by `token_env` (`GITHUB_TOKEN` in the sample).

## Usage

```sh
./ruyi-deck --help
./ruyi-deck repo sync rolling --target rva23 --dry-run
./ruyi-deck repo archive rolling --target rva23 --date 2026-11-12
./ruyi-deck releases create dailybuild --target rva23 --date 2026-11-12
```

Omit `--target` to process all enabled targets. Dates default to today in the
configured timezone. Add `--dry-run` to preview an operation without making changes.

Incremental repository sync uses `--delete` and has no rollback. Existing release
targets are never overwritten; missing targets can be added to the same date.

## License

[MulanPSL-2.0](LICENSE)
