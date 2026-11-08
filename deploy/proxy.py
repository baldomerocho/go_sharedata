#!/usr/bin/env python3
"""Entrada TCP local estable hacia el ClusterIP, sin fijar un pod concreto."""
import asyncio
import sys

async def pipe(reader, writer):
    while data := await reader.read(65536):
        writer.write(data)
        await writer.drain()

async def connection(reader, writer):
    upstream = None
    tasks = []
    try:
        other_reader, upstream = await asyncio.wait_for(asyncio.open_connection(sys.argv[1], 8844), 5)
        tasks = [asyncio.create_task(pipe(reader, upstream)), asyncio.create_task(pipe(other_reader, writer))]
        await asyncio.wait(tasks, return_when=asyncio.FIRST_COMPLETED)
    except (OSError, asyncio.TimeoutError):
        pass
    finally:
        for task in tasks:
            task.cancel()
        await asyncio.gather(*tasks, return_exceptions=True)
        writer.close()
        if upstream:
            upstream.close()

async def main():
    server = await asyncio.start_server(connection, '127.0.0.1', 8844)
    async with server:
        await server.serve_forever()

asyncio.run(main())
