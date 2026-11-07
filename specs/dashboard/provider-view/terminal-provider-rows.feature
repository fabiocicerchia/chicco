@domain:dashboard @area:provider-view @from:proxy/uiprovider
Feature: Seeing each provider's state and quota use in the terminal dashboard
  Lets the operator see at a glance which providers can serve traffic, why the others cannot, and how close each provider and model is to its quota.

  Background:
    Given the operator is watching the terminal dashboard

  @id:dashboard.provider-view.001 @rule:dashboard.provider-view.status-shows-why-provider-cannot-serve @inferred @source:internal/proxy/uiprovider.go#providerStatus
  Scenario Outline: A provider's row shows whether it can serve and, if not, why
    Given a provider "groq" that is <state>
    When the dashboard refreshes
    Then the "groq" row shows the status mark "<mark>" in <colour>
    And the last cell of the row reads "<tail>"

    Examples:
      | state                                               | mark | colour | tail                                  |
      | missing its api_key or models                       | ●    | grey   | not configured — check api_key/models |
      | failing authentication                              | ●    | grey   | auth failed — check API key           |
      | unreachable                                         | ●    | grey   | unreachable                           |
      | not yet probed                                      | ○    | grey   | checking…                             |
      | benched on a rate limit that reopens at 14:30 today | ◐    | amber  | limit · resets 14:30                  |
      | cooling down for 47s after an error                 | ◐    | amber  | its usage bar followed by "cd 47s"    |
      | healthy                                             | ●    | green  | its usage bar and percentage          |

  @id:dashboard.provider-view.002 @rule:dashboard.provider-view.usage-against-governing-quota @verified @source:internal/proxy/uiprovider.go#usageBar @test:internal/proxy/ui_test.go#TestProviderRowsQuotaCells
  Scenario Outline: A provider's usage is shown against its token quota, else its request quota, else as unquota'd
    Given a provider "groq" with <quota> that has used <used> in the current window
    When the dashboard refreshes
    Then the "groq" row's usage cell reads "<cell>"
    And the row's bar reads <bar>

    Examples:
      | quota                  | used                       | cell       | bar                         |
      | a quota of 1000 tokens | 250 tokens over 4 requests | 250 / 1.0k | a quarter-filled bar at 25% |
      | a quota of 10 requests | 6 requests                 | 6 / 10 req | a bar at 60%                |
      | no quota               | 1500 tokens                | 1.5k / —   | (no quota)                  |

  @id:dashboard.provider-view.003 @rule:dashboard.provider-view.one-row-per-model @verified @source:internal/proxy/uiprovider.go#providerRows @test:internal/proxy/ui_test.go#TestProviderRowsQuotaCells
  Scenario Outline: Each provider is listed with one row per configured model
    Given a provider "groq" configured with the models <models>
    When the dashboard refreshes
    Then "groq" takes <count> rows
    And the first row carries the provider name, its kind, the model "<first>" and the provider's own usage and request count
    And each further row shows only a model name with that model's request count

    Examples:
      | models                                    | count | first        |
      | llama-3.1-8b, llama-3.3-70b, mixtral-8x7b | 3     | llama-3.1-8b |
      | none                                      | 1     | —            |

  @id:dashboard.provider-view.004 @rule:dashboard.provider-view.model-own-quota @verified @source:internal/proxy/uiprovider.go#modelCells @test:internal/proxy/ui_test.go#TestProviderRowsQuotaCells @questionable
  Scenario Outline: A model with its own quota is measured against that quota, except the provider's first model
    Given a provider "groq" with a quota of 1000 tokens
    And its model "<model>" has its own quota of <model quota> and has used <model used> in that window
    And "<model>" is the <position> model of "groq"
    When the dashboard refreshes
    Then the "<model>" row's usage cell reads "<cell>"

    Examples:
      | model         | model quota | model used | position | cell                              |
      | llama-3.3-70b | 100 tokens  | 50 tokens  | second   | 50 / 100                          |
      | mixtral-8x7b  | 10 requests | 6 requests | third    | 6 / 10 req                        |
      | llama-3.1-8b  | 100 tokens  | 50 tokens  | first    | the provider's usage against 1.0k |

  @id:dashboard.provider-view.005 @rule:dashboard.provider-view.model-falls-back-to-provider-quota @inferred @source:internal/proxy/uiprovider.go#modelCells @questionable
  Scenario: A model without its own quota is measured against the provider's quota using its all-time totals
    Given a provider "groq" with a daily quota of 1000 tokens
    And its second model "llama-3.3-70b" has no quota of its own and has used 1200 tokens since chicco started, 200 of them today
    When the dashboard refreshes
    Then the "llama-3.3-70b" row's usage cell reads "1.2k / 1.0k"
    And its bar is full at 120%
