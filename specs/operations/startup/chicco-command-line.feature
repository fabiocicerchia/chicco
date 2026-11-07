@domain:operations @area:startup @from:cmd/chicco
Feature: Running and checking chicco from the command line
  Gives the operator one command to start the proxy, validate a chicco.yaml before deploying it, and see which build is installed.

  @id:operations.startup.001 @rule:operations.cli.config-check-reports-validity @verified @source:cmd/chicco/main.go#checkConfig @test:cmd/chicco/main_test.go#TestCheckAcceptsAnInactiveProvider
  Scenario Outline: Checking a config reports whether it is sound without starting the proxy
    Given a chicco.yaml that is <config_state>
    When the operator runs chicco -check against it
    Then no server is started and no port is bound
    And chicco prints <output>
    And chicco exits with code <exit_code>

    Examples:
      | config_state                                               | output                                       | exit_code |
      | free of problems                                           | "chicco: chicco.yaml is valid" on stdout     | 0         |
      | valid apart from a provider whose api_key expands to empty | the warning on stderr, prefixed "chicco:"    | 0         |
      | declaring a provider with a hard validation problem        | each problem on stderr, prefixed "chicco:"   | 1         |
      | missing or not parseable as YAML                           | the load error on stderr, prefixed "chicco:" | 1         |

  @id:operations.startup.002 @rule:operations.cli.strict-check-fails-on-warnings @verified @source:cmd/chicco/main.go#checkConfig @test:cmd/chicco/main_test.go#TestStrictCheckFailsOnAnInactiveProvider @test:cmd/chicco/main_test.go#TestStrictCheckPassesAValidConfig
  Scenario Outline: A strict config check fails on warnings as well as errors
    Given a chicco.yaml that <config_state>
    When the operator runs chicco -check -strict against it
    Then chicco exits with code <exit_code>

    Examples:
      | config_state                                                          | exit_code |
      | declares a provider that cannot activate because its api_key is unset | 1         |
      | has no warnings and no errors                                         | 0         |

  @id:operations.startup.003 @rule:operations.cli.version-prints-build @inferred @source:cmd/chicco/main.go#main
  Scenario: The version flag prints the build version and exits
    Given a chicco binary built with version 1.4.0
    When the operator runs chicco -version
    Then chicco prints "chicco 1.4.0" on stdout
    And chicco exits with code 0 without starting the proxy

  @id:operations.startup.004 @rule:operations.cli.startup-failure-exits-nonzero @inferred @source:cmd/chicco/main.go#main
  Scenario: A proxy that cannot start reports why and exits with a failure code
    Given chicco cannot start, for example because its listen address is already in use
    When the operator runs chicco
    Then chicco prints the reason on stderr, prefixed "chicco:"
    And chicco exits with code 1

  @id:operations.startup.005 @rule:operations.cli.help-shows-usage @inferred @source:cmd/chicco/main.go#usage
  Scenario: Help explains what chicco does and where to point clients
    When the operator runs chicco -h
    Then chicco prints a description of the proxy and the list of flags with their defaults on stderr
    And the help shows the example "chicco -config chicco.yaml -addr :41986"
    And the help tells the operator to point an OpenAI client at http://127.0.0.1:41986/v1
