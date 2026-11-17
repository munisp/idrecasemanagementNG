# Author: Patrick Munis at newwave
"""Cross-tenant negative tests for the analytics query service.

The API side is pinned by Go's tenant_isolation_test.go; this side pins the
DataFusion gateway's own scope resolution: an X-Tenant-Code header can only
narrow or match the token's scope, never widen it, and the SQL gate must not
let a query escape its tenant partition by shaping.
"""
import pytest

from datafusion_service import ADMIN_ROLES, AuthError, resolve_tenant
from lakehouse_common import validate_readonly_sql, wrap_with_row_cap


class TestResolveTenant:
    def test_header_matching_claim_allowed(self):
        assert resolve_tenant("tx", {"CASE_MANAGER"}, "tx") == "tx"

    def test_header_mismatch_without_admin_rejected(self):
        with pytest.raises(AuthError):
            resolve_tenant("tx", {"CASE_MANAGER"}, "ny")

    def test_no_header_uses_claim(self):
        assert resolve_tenant("tx", set(), None) == "tx"

    def test_no_scope_at_all_rejected(self):
        with pytest.raises(AuthError):
            resolve_tenant(None, {"CASE_MANAGER"}, None)

    def test_header_without_claim_rejected(self):
        # a header must never substitute for a missing token scope
        with pytest.raises(AuthError):
            resolve_tenant(None, {"CASE_MANAGER"}, "tx")

    def test_admin_may_cross_scope(self):
        for role in ADMIN_ROLES:
            assert resolve_tenant("tx", {role}, "ny") == "ny"

    def test_non_admin_similar_names_rejected(self):
        # STATE_AUDITOR reads everywhere at the API layer, but the analytics
        # gateway honors only the explicit admin set -- pin that.
        with pytest.raises(AuthError):
            resolve_tenant("tx", {"STATE_AUDITOR"}, "ny")

    def test_malformed_codes_rejected(self):
        for bad in ["tex", "t1", "../x", " tx", "tx;drop"]:
            with pytest.raises(AuthError):
                resolve_tenant("tx", set(), bad)

    def test_malformed_claim_rejected(self):
        with pytest.raises(AuthError):
            resolve_tenant("tex", set(), None)


class TestSqlGateTenantEscape:
    """The row-level guarantee comes from partition-filtered registration,
    but the SQL gate is the second wall: nothing that mutates or reaches
    outside the registered marts may pass."""

    def test_attach_rejected(self):
        reason = validate_readonly_sql("attach 's3://other-tenant/lake' as x")
        assert reason is not None

    def test_copy_export_rejected(self):
        reason = validate_readonly_sql("copy (select * from gold.qpa_benchmarks) to '/tmp/x.csv'")
        assert reason is not None

    def test_comment_smuggled_write_rejected(self):
        # 'dr/**/op' strips to 'drop' after comment removal
        reason = validate_readonly_sql("select 1; dr/**/op table gold.qpa_benchmarks")
        assert reason is not None

    def test_second_statement_rejected(self):
        reason = validate_readonly_sql("select 1; select 2")
        assert reason is not None

    def test_clean_select_accepted(self):
        assert validate_readonly_sql("select tenant, avg(qpa_usd) from gold.qpa_benchmarks group by 1") is None

    def test_row_cap_wraps(self):
        capped = wrap_with_row_cap("select * from gold.qpa_benchmarks", 100)
        assert "limit 101" in capped.lower()  # cap+1 so truncation is detectable
