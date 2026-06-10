# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

ProteOS is a web-based "desktop OS" that spawns AI coding CLIs (Claude Code, Gemini CLI, OpenAI Codex) inside isolated Docker containers and exposes each as a browser terminal. The orchestrator server manages the container lifecycle; the browser UI renders draggable windows whose contents are `ttyd` web terminals served from each container.
