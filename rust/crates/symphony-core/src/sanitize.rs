//! SPEC §4.2 / §9.5 workspace key sanitization.

/// Replace any character outside `[A-Za-z0-9._-]` with `_`.
pub fn workspace_key(identifier: &str) -> String {
    identifier
        .chars()
        .map(|c| match c {
            'A'..='Z' | 'a'..='z' | '0'..='9' | '.' | '_' | '-' => c,
            _ => '_',
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn keeps_safe_characters() {
        assert_eq!(workspace_key("MT-649"), "MT-649");
        assert_eq!(workspace_key("ABC.123_x"), "ABC.123_x");
    }

    #[test]
    fn replaces_unsafe_characters() {
        assert_eq!(workspace_key("foo bar"), "foo_bar");
        assert_eq!(workspace_key("a/b\\c"), "a_b_c");
        assert_eq!(workspace_key("héllo"), "h_llo");
    }
}
