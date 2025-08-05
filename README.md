# Paketo Buildpack for Yarn

The Yarn CNB provides the [Yarn Package manager](https://yarnpkg.com). The
buildpack installs `yarn` onto the `$PATH` which makes it available for
subsequent buildpacks and/or in the final running container. An example of
buildpack that might use yarn is the [Yarn Install
CNB](https://github.com/paketo-buildpacks/yarn-install)

## Integration

The Yarn CNB provides `yarn` as dependency. Downstream buildpacks, like [Yarn
Install CNB](https://github.com/paketo-buildpacks/yarn-install) can require the
`yarn` dependency by generating a [Build Plan
TOML](https://github.com/buildpacks/spec/blob/master/buildpack.md#build-plan-toml)
file that looks like the following:

```toml
[[requires]]

  # The name of the Yarn dependency is "yarn". This value is considered
  # part of the public API for the buildpack and will not change without a plan
  # for deprecation.
  name = "yarn"

  # The Yarn buildpack supports some non-required metadata options.
  [requires.metadata]

    # The version of the Yarn dependency is not required. In the case it
    # is not specified, the buildpack will provide the default version, which can
    # be seen in the buildpack.toml file.
    # If you wish to request a specific version, the buildpack supports
    # specifying a semver constraint in the form of "1.*", "1.22.*", or even
    # "1.22.4".
    version = "1.22.4"

    # Setting the build flag to true will ensure that the yarn
    # depdendency is available on the $PATH for subsequent buildpacks during
    # their build phase. If you are writing a buildpack that needs to run yarn
    # during its build process, this flag should be set to true.
    build = true

    # Setting the launch flag to true will ensure that the yarn
    # dependency is available on the $PATH for the running application. If you are
    # writing an application that needs to run yarn at runtime, this flag should
    # be set to true.
    launch = true
```

## Configuration

### BP_YARN_VERSION
The `BP_YARN_VERSION` variable allows you to specify the version of Yarn that the buildpack will provide.

```shell
BP_YARN_VERSION=4.9.2
```

This will cause the buildpack to provide Yarn v4.9.2. Any valid semver version can be specified.

The buildpack supports both Yarn Classic (1.x) and Yarn Berry (2.x, 3.x, 4.x+) versions. The version selection follows this priority order:

1. `BP_YARN_VERSION` environment variable
2. `version` in the Build Plan metadata
3. Default version specified in `buildpack.toml`

#### Supported Versions

The buildpack includes the following Yarn versions:

**Yarn Classic:**
- 1.22.* (all patch versions)

**Yarn Berry:**  
- 2.4.3, 3.8.7, 4.0.2, 4.9.2

For the complete list of available versions, see the `buildpack.toml` file.

## Usage

To package this buildpack for consumption:

```shell
$ ./scripts/package.sh --version <version-number>
```

This will create a `buildpackage.cnb` file under the `build` directory which you
can use to build your app as follows:
```shell
pack build <app-name> \
  --path <path-to-app> \
  --buildpack <path/to/node-engine.cnb> \
  --buildpack build/buildpackage.cnb \
  --buildpack <path/to/cnb-that-requires-node-and-yarn>
```

Though the API of this buildpack does not require `node`, yarn is unusable without node.

## Run Tests

To run all unit tests, run:
```shell
./scripts/unit.sh
```

To run all integration tests, run:
```shell
/scripts/integration.sh
```
