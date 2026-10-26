"""Exercise every generated facade method against a real gRPC service.

The facade supplies procedure, message type and RPC shape. Transport selection
belongs to its supplied Gateway, as it does for every generated Python client.
"""
from concurrent import futures

import grpc
from shapes_client.shapes import shapes
from shapes_client._gen import shapes_pb2 as pb
from shapes_client._gen import shapes_pb2_grpc as rpc


class Echo(rpc.EchoServiceServicer):
    def Echo(self, request, context):
        return request

    def Fan(self, request, context):
        for suffix in ("1", "2", "3"):
            yield pb.Message(text=request.text + suffix)

    def Gather(self, requests, context):
        return pb.Message(text=",".join(request.text for request in requests))

    def Chat(self, requests, context):
        for request in requests:
            yield pb.Message(text=request.text.upper())


class Gateway:
    """The existing gateway seam contract over grpcio's four multicallables."""
    def __init__(self, channel):
        self.channel = channel

    def call(self, shape, procedure, request, response_type):
        return getattr(self.channel, shape)(
            procedure,
            request_serializer=lambda message: message.SerializeToString(),
            response_deserializer=response_type.FromString,
        )(request, timeout=10)

    def unary(self, procedure, request, response_type):
        return self.call("unary_unary", procedure, request, response_type)

    def unary_stream(self, procedure, request, response_type):
        return self.call("unary_stream", procedure, request, response_type)

    def stream_unary(self, procedure, requests, response_type):
        return self.call("stream_unary", procedure, requests, response_type)

    def stream_stream(self, procedure, requests, response_type):
        return self.call("stream_stream", procedure, requests, response_type)


server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
rpc.add_EchoServiceServicer_to_server(Echo(), server)
port = server.add_insecure_port("127.0.0.1:0")
server.start()
try:
    with grpc.insecure_channel(f"127.0.0.1:{port}") as channel:
        client = shapes(Gateway(channel)).echo()
        assert client.echo(pb.Message(text="one")).text == "one"
        assert [r.text for r in client.fan(pb.Message(text="item"))] == ["item1", "item2", "item3"]
        assert client.gather(iter(pb.Message(text=x) for x in ("a", "b", "c"))).text == "a,b,c"
        assert client.gather(iter(())).text == ""
        assert [r.text for r in client.chat(iter(pb.Message(text=x) for x in ("a", "b", "c")))] == ["A", "B", "C"]
        assert list(client.chat(iter(()))) == []
finally:
    server.stop(None).wait()
print("all four generated RPC shapes passed")
