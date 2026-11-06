#!/usr/bin/env python3
"""Regression-test the exact port policy used by the owned fixture launcher."""
import importlib.util
import pathlib
import unittest

spec = importlib.util.spec_from_file_location(
    "supabase_ports", pathlib.Path(__file__).parent / "lib" / "supabase-ports.py"
)
policy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(policy)


def binding(address="127.0.0.1", port="43210"):
    return {"HostIp": address, "HostPort": port}


class SupabasePortPolicyTest(unittest.TestCase):
    def test_internal_expose_without_publication_is_allowed(self):
        for ports in (None, {}, {"5432/tcp": None}, {"3000/tcp": []}):
            with self.subTest(ports=ports):
                policy.assert_no_host_ports("owned internal service", ports)

    def test_actual_internal_publication_is_refused_even_on_loopback(self):
        with self.assertRaises(ValueError):
            policy.assert_no_host_ports("owned PostgreSQL", {"5432/tcp": [binding()]})

    def test_kong_unbound_exposed_admin_and_tls_ports_do_not_count_as_published(self):
        policy.assert_kong_proxy("43210", {
            "8000/tcp": [binding()], "8001/tcp": None, "8443/tcp": [], "8444/tcp": None,
        }, ["KONG_ADMIN_LISTEN=off"])

    def test_actual_kong_admin_publication_is_refused(self):
        with self.assertRaises(ValueError):
            policy.assert_kong_proxy("43210", {
                "8000/tcp": [binding()], "8001/tcp": [binding(port="43211")],
            }, ["KONG_ADMIN_LISTEN=off"])

    def test_multiple_proxy_bindings_are_refused(self):
        with self.assertRaises(ValueError):
            policy.assert_kong_proxy("43210", {"8000/tcp": [binding(), binding("::1")]}, ["KONG_ADMIN_LISTEN=off"])

    def test_non_loopback_wrong_port_and_missing_proxy_are_refused(self):
        for ports in ({"8000/tcp": [binding("0.0.0.0")]},
                      {"8000/tcp": [binding(port="43211")]}, {"8000/tcp": None}):
            with self.subTest(ports=ports), self.assertRaises(ValueError):
                policy.assert_kong_proxy("43210", ports, ["KONG_ADMIN_LISTEN=off"])

    def test_admin_listener_must_stay_off(self):
        for environment in ([], ["KONG_ADMIN_LISTEN=0.0.0.0:8001"], ["KONG_ADMIN_LISTEN="]):
            with self.subTest(environment=environment), self.assertRaises(ValueError):
                policy.assert_kong_proxy("43210", {"8000/tcp": [binding()]}, environment)

    def test_malformed_inspect_shapes_fail_closed(self):
        for ports in ([], "", {"8000/tcp": "not-a-binding"}, {"8000/tcp": {}}):
            with self.subTest(ports=ports), self.assertRaises(ValueError):
                policy.published_bindings(ports)


if __name__ == "__main__":
    unittest.main()
