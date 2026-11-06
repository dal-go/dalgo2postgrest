#!/usr/bin/env python3
"""Validate actual Docker host bindings, ignoring unbound image EXPOSE entries."""
import json
import sys


def published_bindings(ports):
    """Docker uses null/[] for exposed ports with no actual host publication."""
    if ports is None:
        return {}
    if not isinstance(ports, dict):
        raise ValueError("Docker port bindings must be an object")
    published = {}
    for port, bindings in ports.items():
        if bindings is None or bindings == []:
            continue
        if not isinstance(bindings, list):
            raise ValueError("Docker port bindings must be lists or null")
        published[port] = bindings
    return published


def assert_no_host_ports(name, ports):
    if published_bindings(ports):
        raise ValueError("%s publishes host ports, want none" % name)


def assert_kong_proxy(port, ports, environment):
    published = published_bindings(ports)
    if set(published) != {"8000/tcp"}:
        raise ValueError("owned Kong must publish only its proxy port")
    bindings = published["8000/tcp"]
    if len(bindings) != 1 or not isinstance(bindings[0], dict):
        raise ValueError("owned Kong must publish exactly one proxy binding")
    binding = bindings[0]
    if binding.get("HostIp") != "127.0.0.1" or binding.get("HostPort") != port:
        raise ValueError("owned Kong proxy must bind its exact owned 127.0.0.1 port")
    if not isinstance(environment, list) or any(not isinstance(entry, str) for entry in environment):
        raise ValueError("owned Kong environment must be a string list")
    env = dict(entry.partition("=")[::2] for entry in environment)
    if env.get("KONG_ADMIN_LISTEN") != "off":
        raise ValueError("owned Kong admin listener is not off")


def main(arguments):
    if len(arguments) == 3 and arguments[0] == "none":
        assert_no_host_ports(arguments[1], json.loads(arguments[2]))
    elif len(arguments) == 4 and arguments[0] == "kong":
        assert_kong_proxy(arguments[1], json.loads(arguments[2]), json.loads(arguments[3]))
    else:
        raise ValueError("expected none NAME PORTS_JSON or kong PORT PORTS_JSON ENV_JSON")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except (ValueError, TypeError) as error:
        raise SystemExit(str(error)) from None
