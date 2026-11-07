@domain:configuration @area:hot-reload @from:proxy/reload-unix
Feature: Reloading the config with a hangup signal
  Lets an operator on Unix apply chicco.yaml edits to a running proxy, without a restart or dropped callers, by sending it SIGHUP.

  Background:
    Given chicco is running on a Unix host from the config file chicco.yaml

  @id:configuration.hot-reload.005 @rule:configuration.hot-reload.sighup-applies-edits @inferred @source:internal/proxy/reload_unix.go#watchSIGHUP
  Scenario: A hangup signal applies config edits without a restart
    Given the operator has edited chicco.yaml so that it now lists the providers "groq" and "openrouter"
    When the operator sends chicco a SIGHUP
    Then chicco keeps running in the same process
    And requests are now routed across "groq" and "openrouter"
    And the log shows "chicco: config reloaded from chicco.yaml (2 provider(s))"

  @id:configuration.hot-reload.006 @rule:configuration.hot-reload.bad-edit-keeps-running-config @inferred @source:internal/proxy/reload.go#reloadFromFile
  Scenario Outline: A broken config edit sent by hangup signal leaves the running config in place
    Given chicco is serving with the providers "groq" and "openrouter"
    And the operator has saved chicco.yaml with <problem>
    When the operator sends chicco a SIGHUP
    Then chicco keeps serving with "groq" and "openrouter"
    And the log shows a line starting with "<log prefix>"

    Examples:
      | problem                                                        | log prefix                                     |
      | a YAML syntax error                                            | chicco: reload failed, keeping current config: |
      | a hard validation problem, such as a provider with no base URL | chicco: reload rejected —                      |
