@domain:operations @area:installation @from:chicco/install
Feature: Installing chicco with the install script
  Lets an operator on Linux or macOS install a verified chicco release binary with one command, without building from source.

  Background:
    Given chicco releases publish one archive per platform and a checksums.txt file

  @id:operations.installation.001 @rule:operations.install.platform-archive-selection @inferred @source:install.sh#archive
  Scenario Outline: The installer picks the release archive matching the machine's OS and architecture
    Given the operator's machine reports OS '<os>' and architecture '<machine>'
    And CHICCO_VERSION is set to 'v1.4.0'
    When the operator runs the install script
    Then the installer prints 'chicco-install: downloading <archive> (v1.4.0)'
    And the archive is fetched from https://github.com/fabiocicerchia/chicco/releases/download/v1.4.0/<archive>

    Examples:
      | os     | machine | archive                          |
      | Linux  | x86_64  | chicco_1.4.0_linux_amd64.tar.gz  |
      | Linux  | aarch64 | chicco_1.4.0_linux_arm64.tar.gz  |
      | Darwin | arm64   | chicco_1.4.0_darwin_arm64.tar.gz |
      | Darwin | amd64   | chicco_1.4.0_darwin_amd64.tar.gz |

  @id:operations.installation.002 @rule:operations.install.unsupported-platform-refused @inferred @source:install.sh#err
  Scenario Outline: The installer refuses unsupported operating systems and architectures
    Given the operator's machine reports OS '<os>' and architecture '<machine>'
    When the operator runs the install script
    Then the installer exits with an error
    And it prints '<message>'
    And nothing is downloaded or installed

    Examples:
      | os              | machine | message                                                                                                                                |
      | MINGW64_NT-10.0 | x86_64  | chicco-install: unsupported OS 'mingw64_nt-10.0' — on Windows download the .zip from https://github.com/fabiocicerchia/chicco/releases |
      | FreeBSD         | amd64   | chicco-install: unsupported OS 'freebsd' — on Windows download the .zip from https://github.com/fabiocicerchia/chicco/releases         |
      | Linux           | armv7l  | chicco-install: unsupported architecture 'armv7l'                                                                                      |

  @id:operations.installation.003 @rule:operations.install.checksum-verified-before-install @inferred @source:install.sh#want
  Scenario Outline: A download whose checksum does not match the published checksums is never installed
    Given the release v1.4.0 publishes checksums.txt
    And the downloaded archive <condition>
    When the operator runs the install script
    Then the installer exits with an error
    And it prints '<message>'
    And no chicco binary is written to the install directory

    Examples:
      | condition                                            | message                                                               |
      | has a SHA-256 different from its checksums.txt entry | chicco-install: checksum mismatch — expected <published> got <actual> |
      | has no entry in checksums.txt                        | chicco-install: no checksum for chicco_1.4.0_linux_amd64.tar.gz       |

  @id:operations.installation.004 @rule:operations.install.prerequisite-tools-required @inferred @source:install.sh#have
  Scenario Outline: The installer refuses to run without a downloader or a SHA-256 tool
    Given the operator's machine has <missing>
    When the operator runs the install script
    Then the installer exits with an error
    And it prints '<message>'
    And no chicco binary is installed

    Examples:
      | missing                      | message                                                         |
      | neither curl nor wget        | chicco-install: need curl or wget                               |
      | neither sha256sum nor shasum | chicco-install: need sha256sum or shasum to verify the download |

  @id:operations.installation.005 @rule:operations.install.version-selection @inferred @source:install.sh#version
  Scenario Outline: The installer installs the pinned version, or the latest release when none is pinned
    Given <pinning>
    And the latest published release is v1.5.0
    And BINDIR is not set and /usr/local/bin is writable
    When the operator runs the install script
    Then <outcome>

    Examples:
      | pinning                                                              | outcome                                                                                          |
      | CHICCO_VERSION is set to 'v1.4.0'                                    | the installer prints 'chicco-install: installed /usr/local/bin/chicco (v1.4.0)'                  |
      | CHICCO_VERSION is not set                                            | the installer prints 'chicco-install: installed /usr/local/bin/chicco (v1.5.0)'                  |
      | CHICCO_VERSION is not set and the latest release cannot be looked up | the installer exits with 'chicco-install: could not resolve latest version — set CHICCO_VERSION' |

  @id:operations.installation.006 @rule:operations.install.install-directory @inferred @source:install.sh#bindir @questionable
  Scenario Outline: The binary goes to /usr/local/bin when writable, to ~/.local/bin otherwise, or to the directory the operator chose
    Given <setup>
    When the operator runs the install script
    Then <outcome>

    Examples:
      | setup                                                       | outcome                                                                                           |
      | BINDIR is not set and /usr/local/bin is writable            | chicco is installed as /usr/local/bin/chicco with mode 0755                                       |
      | BINDIR is not set and /usr/local/bin is not writable        | chicco is installed as ~/.local/bin/chicco with mode 0755                                         |
      | BINDIR is set to '/opt/tools/bin' and it is writable        | chicco is installed as /opt/tools/bin/chicco with mode 0755                                       |
      | BINDIR is set to '/opt/locked/bin', which exists but is not writable | the installer exits with 'chicco-install: cannot write to /opt/locked/bin — set BINDIR or re-run with sudo' |

  @id:operations.installation.007 @rule:operations.install.path-hint @inferred @source:install.sh#bindir
  Scenario Outline: The installer tells the operator when the install directory is not on their PATH
    Given the operator's home directory is /home/alex
    And chicco was installed into ~/.local/bin
    And ~/.local/bin is <onpath> on the operator's PATH
    When the operator runs the install script
    Then the installer <hint>

    Examples:
      | onpath  | hint                                                            |
      | not     | prints 'chicco-install: add /home/alex/.local/bin to your PATH' |
      | already | prints no PATH hint                                             |
