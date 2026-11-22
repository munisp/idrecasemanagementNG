# Author: Patrick Munis at newwave
# Minimal WorkflowService test double for the DAST harness: serves gRPC on
# 7233 and answers UNIMPLEMENTED for every RPC. Temporal sdk-go v1.30.0 treats
# UNIMPLEMENTED from GetSystemInfo as success (internal_workflow_client.go:
# "We ignore unimplemented"), so case-api boots; any real workflow execution
# fails loudly with UNIMPLEMENTED instead of being silently faked.
#
# Prefer a real `temporal server start-dev` when the CLI is available; this
# stub exists for sandboxes where the dev server cannot be downloaded.
import sys
from concurrent import futures

import grpc


class UnimplementedHandler(grpc.GenericRpcHandler):
    def service(self, handler_call_details):
        def abort(request, context):
            context.abort(grpc.StatusCode.UNIMPLEMENTED,
                          "dast temporal stub: workflow execution unavailable")
        return grpc.unary_unary_rpc_method_handler(
            abort,
            request_deserializer=lambda b: b,
            response_serializer=lambda b: b,
        )


def main():
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    server.add_generic_rpc_handlers((UnimplementedHandler(),))
    server.add_insecure_port("127.0.0.1:7233")
    server.start()
    print("temporal stub listening on 127.0.0.1:7233", flush=True)
    server.wait_for_termination()


if __name__ == "__main__":
    sys.exit(main())
