@domain:configuration @area:config-validation @from:proxy/validate
Feature: Checking a config before it ships
  Lets an operator run `chicco -check` (in CI, a pre-commit hook or by hand) to catch config mistakes that would leave providers unused or fields ignored, and stops a config with broken aliases from loading at all.

  Background:
    Given Given the operator keeps the proxy's settings in chicco.yaml

  @id:configuration.config-validation.001 @rule:configuration.check.sound-config-passes @verified @source:internal/proxy/validate.go#Validate @test:internal/proxy/config_test.go#TestValidateCleanConfig @test:cmd/chicco/main_test.go#TestStrictCheckPassesAValidConfig
  Scenario Outline: A sound config passes the check, even in strict mode
    Given chicco.yaml declares an http provider "groq" with a base_url, an api_key and models
    And chicco.yaml declares a cli provider "claude" with a command and models
    When the operator runs chicco -check <strict_flag>
    Then the output is "chicco: chicco.yaml is valid"
    And the command exits with status 0
    And no server is started and no port is bound

    Examples:
      | strict_flag     |
      | without -strict |
      | with -strict    |

  @id:configuration.config-validation.002 @rule:configuration.check.hard-errors-fail @verified @source:internal/proxy/validate.go#validateProvider @test:internal/proxy/config_test.go#TestValidateCatchesMistakes @test:internal/proxy/config_test.go#TestValidateModelStrategy @test:internal/proxy/config_test.go#TestValidateBackendWeight @questionable
  Scenario Outline: A config mistake that leaves a setting broken fails the check and is named
    Given chicco.yaml contains <mistake>
    When the operator runs chicco -check
    Then the error output contains "chicco: <message>"
    And the command exits with status 1

    Examples:
      | mistake                                                  | message                                                                                   |
      | provider "bad-kind" with kind: grpc                      | bad-kind: unknown kind "grpc" (want "http" or "cli")                                      |
      | provider "bad-out" with output: yaml                     | bad-out: unknown output "yaml" (want "text" or "json")                                    |
      | provider "bad-json" with output: json and no result_path | bad-json: output: json needs a result_path                                                |
      | cli provider "cli-nocmd" with no command                 | cli-nocmd: kind: cli needs a command                                                      |
      | http provider "http-nourl" with no base_url              | http-nourl: missing base_url                                                              |
      | two providers both named "groq"                          | groq: duplicate provider name                                                             |
      | the third provider has no name                           | providers[2]: missing name                                                                |
      | provider "groq" with timeout_seconds: -5                 | groq: timeout_seconds must not be negative                                                |
      | provider "groq" with weight: -1                          | groq: weight must not be negative                                                         |
      | provider "groq" with a negative quota limit              | groq: quota must not be negative                                                          |
      | a negative top-level quota limit                         | quota: must not be negative                                                               |
      | virtual model "fast" with strategy: fifo                 | model fast: unknown strategy "fifo" (want "order", "round_robin", "random" or "weighted") |
      | virtual model "fast" whose backend "groq" has weight: -1 | model fast: backend groq: weight must not be negative                                     |

  @id:configuration.config-validation.003 @rule:configuration.check.inactive-provider-warns @verified @source:internal/proxy/validate.go#validateProviders @test:cmd/chicco/main_test.go#TestCheckAcceptsAnInactiveProvider @test:cmd/chicco/main_test.go#TestStrictCheckFailsOnAnInactiveProvider @test:internal/proxy/config_test.go#TestValidateWarnsInactive
  Scenario Outline: A provider that cannot activate is a warning, and fails the check only in strict mode
    Given chicco.yaml declares a cli provider "local-cli" with a command and models
    And chicco.yaml declares <provider>
    When the operator runs chicco -check <strict_flag>
    Then the error output contains "chicco: warning: <warning>"
    And the command exits with status <exit_code>

    Examples:
      | provider                                                         | warning                                                               | strict_flag     | exit_code |
      | http provider "anthropic-primary" whose api_key env var is empty | anthropic-primary: no api_key (unset env var?) — provider is inactive | without -strict | 0         |
      | http provider "anthropic-primary" whose api_key env var is empty | anthropic-primary: no api_key (unset env var?) — provider is inactive | with -strict    | 1         |
      | http provider "nomodel" with an api_key but no models            | nomodel: no models — provider is inactive                             | without -strict | 0         |
      | http provider "nomodel" with an api_key but no models            | nomodel: no models — provider is inactive                             | with -strict    | 1         |

  @id:configuration.config-validation.004 @rule:configuration.check.no-active-providers-warns @verified @source:internal/proxy/validate.go#validateProviders @test:internal/proxy/config_test.go#TestValidateWarnsInactive @questionable
  Scenario: A config where no provider can activate is flagged
    Given every provider in chicco.yaml is an http provider with no api_key or has no models
    When the operator runs chicco -check
    Then the error output contains "chicco: warning: no active providers (none have both models and, for http, an api_key)"
    And the command exits with status 0

  @id:configuration.config-validation.005 @rule:configuration.check.no-providers-fails @inferred @source:internal/proxy/validate.go#Validate @questionable
  Scenario: A config with no providers fails the check
    Given chicco.yaml declares no providers
    And chicco.yaml declares virtual model "fast" with strategy: fifo
    When the operator runs chicco -check
    Then the error output is exactly "chicco: no providers configured"
    And the unknown strategy of model "fast" is not reported
    And the command exits with status 1

  @id:configuration.config-validation.006 @rule:configuration.aliases.must-target-a-model @verified @source:internal/proxy/validate.go#validateAliases @test:internal/proxy/alias_test.go#TestAliasPointingAtNothingIsRefusedAtLoad @test:internal/proxy/alias_test.go#TestAliasWithAnEmptyTargetIsRefused @test:internal/proxy/alias_test.go#TestAliasCannotShadowAModelID @test:internal/proxy/alias_test.go#TestNoAliasesIsNotAnError
  Scenario Outline: An alias that does not point at a configured model stops the config loading
    Given chicco.yaml declares virtual model "llama-3.3-70b"
    And chicco.yaml declares the alias <alias> pointing at <target>
    When the operator starts chicco or runs chicco -check
    Then chicco refuses the config with "parsing chicco.yaml: <message>"
    And the command exits with status 1

    Examples:
      | alias         | target        | message                                                                       |
      | broken        | no-such-model | alias "broken" points at "no-such-model", which is not a model in this config |
      | empty         | ""            | alias "empty" has no target                                                   |
      | llama-3.3-70b | llama-3.3-70b | alias "llama-3.3-70b" has the same name as a model                            |
      | fast          | chicco:auto   | (none: the config loads)                                                      |
