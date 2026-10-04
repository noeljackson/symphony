import unittest

from check_pr_body import validate


TEMPLATE = "#### Context\n\n<!-- explain -->\n\n#### Summary\n\n- <!-- changes -->\n\n#### Test Plan\n\n- [ ] tests\n"
VALID = "#### Context\n\nRepair dependencies.\n\n#### Summary\n\n- Upgrade dependencies.\n\n#### Test Plan\n\n- [x] Cargo tests\n"


class PRBodyTests(unittest.TestCase):
    def test_valid_body(self):
        self.assertEqual(validate(TEMPLATE, VALID), [])

    def test_missing_heading(self):
        self.assertTrue(validate(TEMPLATE, VALID.replace("#### Context", "Context")))

    def test_heading_order(self):
        self.assertTrue(validate(TEMPLATE, VALID.replace("#### Context", "#### Summary", 1).replace("#### Summary\n\n-", "#### Context\n\n-", 1)))

    def test_placeholder(self):
        self.assertTrue(validate(TEMPLATE, VALID + "\n<!-- unfinished -->"))

    def test_empty_section(self):
        self.assertTrue(validate(TEMPLATE, VALID.replace("Repair dependencies.", "")))

    def test_missing_bullet(self):
        self.assertTrue(validate(TEMPLATE, VALID.replace("- Upgrade", "Upgrade")))

    def test_missing_checkbox(self):
        self.assertTrue(validate(TEMPLATE, VALID.replace("- [x] Cargo", "- Cargo")))

    def test_missing_section_separator(self):
        self.assertTrue(validate(TEMPLATE, VALID.replace("#### Context\n\n", "#### Context\n")))


if __name__ == "__main__":
    unittest.main()
