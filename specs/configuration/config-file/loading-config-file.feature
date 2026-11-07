@domain:configuration @area:config-file @from:proxy/configload
Feature: Loading chicco.yaml
  Lets operators describe the proxy's listen address, providers and secrets in one file, with secrets kept in the environment, so chicco starts with the providers they declared.

  Background:
    Given the operator keeps the proxy configuration in chicco.yaml

  @id:configuration.config-file.006 @rule:configuration.config-file.default-listen-address-is-loopback @inferred @source:internal/proxy/configload.go#LoadConfig
  Scenario: With no listen address configured, chicco listens on loopback only
    Given chicco.yaml sets no addr
    When the operator starts chicco
    Then chicco listens on 127.0.0.1:41986
    And chicco cannot be reached from other machines

  @id:configuration.config-file.007 @rule:configuration.config-file.secrets-and-commands-read-from-environment @inferred @source:internal/proxy/configload.go#LoadConfig
  Scenario Outline: Environment variable references in keys and CLI settings are filled in from the environment
    Given the environment variable <variable> is set to "<value>"
    And chicco.yaml sets <setting> to "<written>"
    When chicco loads the configuration
    Then <setting> takes effect as "<effective>"

    Examples:
      | setting                               | variable     | value             | written                  | effective                  |
      | the proxy's own api_key               | CHICCO_KEY   | sk-proxy-example  | ${CHICCO_KEY}            | sk-proxy-example           |
      | a provider's api_key                  | GROQ_API_KEY | gsk-example       | ${GROQ_API_KEY}          | gsk-example                |
      | a CLI provider's command              | CLAUDE_BIN   | /opt/tools/claude | ${CLAUDE_BIN}            | /opt/tools/claude          |
      | a CLI provider's credential           | HOME         | /home/ops         | ${HOME}/credentials.json | /home/ops/credentials.json |
      | a CLI provider's args entry           | CLI_PROFILE  | work              | --profile=${CLI_PROFILE} | --profile=work             |
      | a CLI provider's health_command entry | CLI_PROFILE  | work              | --profile=$CLI_PROFILE   | --profile=work             |

  @id:configuration.config-file.008 @rule:configuration.config-file.unset-variable-becomes-empty @verified @source:internal/proxy/configload.go#LoadConfig @test:cmd/chicco/main_test.go#TestStrictCheckFailsOnAnInactiveProvider @test:cmd/chicco/main_test.go#TestCheckAcceptsAnInactiveProvider
  Scenario: A key that refers to an unset environment variable loads as empty
    Given the environment variable ANTHROPIC_API_KEY is unset or empty
    And chicco.yaml gives provider "anthropic-primary" the api_key "${ANTHROPIC_API_KEY}"
    When the operator checks the configuration in strict mode
    Then provider "anthropic-primary" has no API key and is reported as inactive
    And the strict check exits with status 1

  @id:configuration.config-file.009 @rule:configuration.config-file.cli-placeholders-survive-expansion @inferred @source:internal/proxy/configload.go#LoadConfig
  Scenario: CLI argument placeholders are left for request time, not filled from the environment
    Given the environment variable "model" is set to "from-shell"
    And a CLI provider's args include "--model={{model}}"
    When chicco loads the configuration
    Then the argument is still "--model={{model}}"
    And each request puts its own model into the argument

  @id:configuration.config-file.010 @rule:configuration.config-file.unreadable-or-malformed-file-refuses-start @inferred @source:internal/proxy/configload.go#LoadConfig
  Scenario Outline: chicco refuses to start when its config file cannot be read or parsed
    Given <condition>
    When the operator starts chicco
    Then chicco prints an error beginning "chicco: <prefix> <path>:"
    And chicco exits with status 1 without serving requests

    Examples:
      | condition                                                 | prefix  | path                    |
      | no file exists at /etc/chicco/chicco.yaml                 | reading | /etc/chicco/chicco.yaml |
      | /etc/chicco/chicco.yaml is not valid YAML                 | parsing | /etc/chicco/chicco.yaml |
      | /etc/chicco/chicco.yaml lists providers as a plain string | parsing | /etc/chicco/chicco.yaml |

  @id:configuration.config-file.011 @rule:configuration.config-file.provider-models-inferred-from-virtual-models @inferred @source:internal/proxy/configload.go#resolveModels
  Scenario: A provider declared without a models list serves the models its virtual-model backends name
    Given http provider "groq" is declared with a base_url and an api_key but without a models list
    And virtual model "llama-3.3-70b" has a backend with provider "groq" and model "llama-70b"
    And virtual model "fast-chat" has a backend with provider "groq" and model "llama-70b" and another with model "llama-8b"
    When chicco loads the configuration
    Then provider "groq" is active with the models "llama-70b" and "llama-8b", in that order
    And "llama-70b" is listed once for "groq"

  @id:configuration.config-file.012 @rule:configuration.config-file.inline-models-extended-by-backends @inferred @source:internal/proxy/configload.go#resolveModels @questionable
  Scenario: A provider's own models list is extended with the models its backends name, even when already listed
    Given provider "groq" lists the models "llama-70b"
    And virtual model "llama-3.3-70b" has a backend with provider "groq" and model "llama-70b"
    When chicco loads the configuration
    Then provider "groq" has the models "llama-70b" and "llama-70b"
    And "llama-70b" appears twice in groq's model rotation and in its model list on the dashboard
