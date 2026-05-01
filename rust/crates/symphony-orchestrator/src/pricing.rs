//! SPEC v2 §13.5: pricing model for converting token counts to USD cost.
//!
//! Symphony does not consume vendor-reported dollar figures (those aren't
//! part of the Codex / Claude Code / OpenAI / Anthropic wire protocols we
//! drive). Instead, the implementation extracts absolute token totals via
//! the §13.5 token-accounting rules and multiplies them by a per-model
//! price drawn from a [`PriceTable`].
//!
//! When the configured backend's pricing is unknown (subscription-priced
//! agents like Codex CLI / Claude Code, or models missing from the table),
//! [`PriceTable::cost_for`] returns `None` and the orchestrator records
//! `cost_usd: None`. Per SPEC §13.5 a `None` cost MUST disable budget-cap
//! enforcement rather than silently treating cost as zero.
//!
//! The built-in table starts empty: the in-tree backends today
//! (`symphony-codex`, `symphony-claude-code`) wrap subscription-billed CLIs
//! and don't emit a model identifier we can price. Once `openai_compat` and
//! `anthropic_messages` land they SHOULD populate this table.

use std::collections::HashMap;

/// Per-million-token rates for a single model.
#[derive(Debug, Clone, Copy)]
pub struct ModelPrice {
    /// USD per million input tokens.
    pub input_per_million: f64,
    /// USD per million output tokens.
    pub output_per_million: f64,
}

impl ModelPrice {
    pub fn new(input_per_million: f64, output_per_million: f64) -> Self {
        Self {
            input_per_million,
            output_per_million,
        }
    }

    pub fn cost(&self, usage: TokenUsage) -> f64 {
        let input = (usage.input_tokens as f64) * self.input_per_million / 1_000_000.0;
        let output = (usage.output_tokens as f64) * self.output_per_million / 1_000_000.0;
        input + output
    }
}

/// Token counts attributable to a single agent turn / accounting tick.
#[derive(Debug, Clone, Copy, Default)]
pub struct TokenUsage {
    pub input_tokens: u64,
    pub output_tokens: u64,
}

/// Lookup table from `(backend, model)` → [`ModelPrice`].
///
/// Backend keys match `AgentBackend::as_str()`. Model keys are matched
/// case-insensitively against the model identifier surfaced by the
/// backend; implementations with model aliases SHOULD register every
/// observed alias rather than relying on prefix matching.
#[derive(Debug, Clone, Default)]
pub struct PriceTable {
    entries: HashMap<(String, String), ModelPrice>,
}

impl PriceTable {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn insert(&mut self, backend: &str, model: &str, price: ModelPrice) {
        self.entries
            .insert((backend.to_string(), model.to_lowercase()), price);
    }

    pub fn cost_for(&self, backend: &str, model: Option<&str>, usage: TokenUsage) -> Option<f64> {
        let model = model?;
        self.entries
            .get(&(backend.to_string(), model.to_lowercase()))
            .map(|p| p.cost(usage))
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub fn len(&self) -> usize {
        self.entries.len()
    }
}

/// SPEC §13.5: built-in price table shipped with this implementation.
///
/// Today this is empty. Stdio backends (codex / claude_code) are
/// subscription-priced and don't emit per-call pricing inputs. The HTTP
/// backends (openai_compat / anthropic_messages), once implemented, MUST
/// populate this table for the models they reasonably expect to dispatch.
pub fn builtin_price_table() -> PriceTable {
    PriceTable::new()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn empty_table_returns_none() {
        let t = builtin_price_table();
        assert!(t.is_empty());
        assert_eq!(
            t.cost_for(
                "openai_compat",
                Some("gpt-5"),
                TokenUsage {
                    input_tokens: 1_000,
                    output_tokens: 500
                }
            ),
            None
        );
    }

    #[test]
    fn cost_computation_matches_per_million_rate() {
        let mut t = PriceTable::new();
        t.insert(
            "openai_compat",
            "demo",
            ModelPrice::new(/* input */ 3.0, /* output */ 15.0),
        );
        let cost = t
            .cost_for(
                "openai_compat",
                Some("DEMO"),
                TokenUsage {
                    input_tokens: 1_000_000,
                    output_tokens: 500_000,
                },
            )
            .unwrap();
        // 1M * $3 + 0.5M * $15 = $3 + $7.50 = $10.50
        assert!((cost - 10.50).abs() < 1e-9, "got {cost}");
    }

    #[test]
    fn unknown_backend_or_model_returns_none() {
        let mut t = PriceTable::new();
        t.insert("openai_compat", "x", ModelPrice::new(1.0, 2.0));
        assert_eq!(
            t.cost_for(
                "anthropic_messages",
                Some("x"),
                TokenUsage {
                    input_tokens: 1,
                    output_tokens: 1
                }
            ),
            None
        );
        assert_eq!(
            t.cost_for(
                "openai_compat",
                Some("missing"),
                TokenUsage {
                    input_tokens: 1,
                    output_tokens: 1
                }
            ),
            None
        );
    }

    #[test]
    fn cost_for_handles_missing_model_name() {
        let mut t = PriceTable::new();
        t.insert("openai_compat", "x", ModelPrice::new(1.0, 2.0));
        assert_eq!(
            t.cost_for(
                "openai_compat",
                None,
                TokenUsage {
                    input_tokens: 1_000_000,
                    output_tokens: 0
                }
            ),
            None
        );
    }
}
