@domain:configuration @area:config-file @from:proxy/config
Feature: Describing providers and settings in chicco.yaml
  Lets an operator declare the provider pool and optional settings in chicco.yaml in whichever layout they prefer, with safe defaults for the listen and metrics addresses.

  Background:
    Given the operator runs chicco with a chicco.yaml config file

  @id:configuration.config-file.001 @rule:config.providers.list-or-keyed-map @inferred @source:internal/proxy/config.go#decodeProviders
  Scenario Outline: Providers can be written as a list or as a map keyed by name
    Given chicco.yaml declares the providers <first> and <second> in the <layout> layout
    When the operator starts chicco
    Then the provider pool contains <first> and <second>
    And <first> comes before <second> in the pool, as in the file

    Examples:
      | layout                              | first | second   |
      | list (each entry has a name: field) | groq  | cerebras |
      | map (the map key is the name)       | groq  | cerebras |

  @id:configuration.config-file.002 @rule:config.providers.map-key-is-default-name @inferred @source:internal/proxy/config.go#decodeProviderMap @questionable
  Scenario Outline: In the map layout a provider's own name field wins over its key
    Given chicco.yaml declares providers in the map layout
    And the entry under the key <key> sets name: <name_field>
    When the operator starts chicco
    Then the provider appears in the pool as <shown_as>

    Examples:
      | key  | name_field | shown_as |
      | groq | (unset)    | groq     |
      | groq | groq-eu    | groq-eu  |

  @id:configuration.config-file.003 @rule:config.providers.reject-non-list-non-map @inferred @source:internal/proxy/config.go#decodeProviders
  Scenario: A providers section that is neither a list nor a map stops the config from loading
    Given chicco.yaml sets providers: groq (a single value, not a list or map)
    When the operator starts chicco
    Then chicco refuses to load the config
    And the error names the file and says providers expected a list or map

  @id:configuration.config-file.004 @rule:config.listen.default-loopback @verified @source:internal/proxy/configload.go#LoadConfig @test:internal/proxy/bind_test.go#TestDefaultAddrIsLoopback
  Scenario: With no address configured chicco listens on loopback only
    Given chicco.yaml sets no addr
    When the operator starts chicco
    Then chicco listens on 127.0.0.1:41986
    And the proxy is not reachable from other machines

  @id:configuration.config-file.005 @rule:config.quota.tracked-window-priority @inferred @source:internal/proxy/config.go#quotaBar
  Scenario Outline: A provider's usage is tracked against its largest configured quota window
    Given provider groq has a quota of <quota>
    When the operator views groq's quota usage
    Then usage is measured as <unit> against a limit of <limit> per <window>

    Examples:
      | quota                   | unit     | limit  | window |
      | tpd 500000 and rpm 30   | tokens   | 500000 | day    |
      | rpd 1000 and tpd 500000 | tokens   | 500000 | day    |
      | rph 200 and tpm 6000    | requests | 200    | hour   |
      | rpm 30 only             | requests | 30     | minute |
      | none                    | no quota | 0      | none   |
