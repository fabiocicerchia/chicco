@domain:configuration @area:hot-reload @from:proxy/reload
Feature: Reloading the config without a restart
  Lets an operator apply chicco.yaml edits (rotated keys, added or removed providers, strategies, aliases, quotas) to a running proxy without losing live usage, cooldowns or the dashboard.

  Background:
    Given chicco is running from chicco.yaml

  @id:configuration.hot-reload.001 @rule:config.hot-reload.applies-edits-on-signal @verified @source:internal/proxy/reload.go#reloadFromFile @test:internal/proxy/reload_test.go#TestReloadPreservesLiveState @test:internal/proxy/alias_test.go#TestReloadPicksUpAliasChanges
  Scenario: A SIGHUP applies chicco.yaml edits to the running proxy
    Given chicco.yaml has been edited to add provider "mistral", set api_key to "sk-proxy-new", set model "smart" to strategy "random" and rename alias "fast" to "quick"
    When the operator sends chicco a SIGHUP
    Then the log shows "chicco: config reloaded from chicco.yaml (2 provider(s))"
    And provider "mistral" appears in the dashboard and is health-probed
    And callers must present "Bearer sk-proxy-new"
    And requests for "smart" are routed with the "random" strategy
    And the alias "quick" resolves and "fast" no longer does

  @id:configuration.hot-reload.002 @rule:config.hot-reload.bad-edit-keeps-running-config @inferred @source:internal/proxy/reload.go#reloadFromFile
  Scenario Outline: A broken edit is refused and the running config stays in force
    Given chicco.yaml has been edited so that <edit>
    When the operator sends chicco a SIGHUP
    Then the log shows <log line>
    And the proxy is <result>

    Examples:
      | edit                                             | log line                                                                                    | result                                       |
      | it is no longer valid YAML                       | "chicco: reload failed, keeping current config: ..."                                        | still serving with the previous config       |
      | alias "fast" points at a model nobody configured | "chicco: reload failed, keeping current config: ..."                                        | still serving with the previous config       |
      | model "smart" uses strategy "fastest"            | "chicco: reload rejected — model smart: unknown strategy ..."                               | still serving with the previous config       |
      | no providers are left                            | "chicco: reload rejected — no providers configured"                                         | still serving with the previous config       |
      | provider "groq" has an empty api_key             | "chicco: reload warning: groq: no api_key (unset env var?) — provider is inactive"          | serving with the new config, "groq" inactive |

  @id:configuration.hot-reload.003 @rule:config.hot-reload.open-bind-auth-not-rechecked @inferred @source:internal/proxy/reload.go#reloadFromFile @questionable
  Scenario: Removing api_key by reload leaves an exposed proxy open
    Given chicco listens on 0.0.0.0:41986 with api_key "sk-proxy-1"
    And chicco.yaml has been edited to remove api_key
    When the operator sends chicco a SIGHUP
    Then the log shows "chicco: config reloaded from chicco.yaml (1 provider(s))"
    And a caller on the network without an Authorization header is served

  @id:configuration.hot-reload.004 @rule:config.hot-reload.unavailable-on-windows @inferred @source:internal/proxy/reload_windows.go#watchSIGHUP @questionable
  Scenario: On Windows a config edit needs a restart
    Given chicco is running on Windows
    And chicco.yaml has been edited to add provider "mistral"
    When the operator saves the file
    Then the dashboard does not list "mistral" until chicco is restarted
