"""Run the operator's aggregate tests and each newly added test explicitly."""
import os
from pathlib import Path
import sys

import delivery_support as support
from verify_merged import run_verification


def verify(arguments):
    if len(arguments) < 2 or arguments[0] != "--":
        raise support.DeliveryError("Pass -- followed by the operator's test command")
    support.discard_prompt()
    source = Path(os.environ.get("TASK_WORKSPACE") or os.getcwd())
    report = []
    try:
        commands = support.added_test_commands(source, report)
        failures = run_verification(source, [arguments[1:]], report)
        failures += run_verification(source, commands, report)
        return 1 if failures else 0
    finally:
        print("\n\n".join(report))


if __name__ == "__main__":
    sys.exit(support.main(verify))
