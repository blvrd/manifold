# Manifold

Manifold runs your stuff. It's directly inspired by , [Solo](https://github.com/aarondfrancis/solo), [foreman](https://github.com/ddollar/foreman?tab=readme-ov-file), and [Overmind](https://github.com/DarthSim/overmind).

## Installation

For now, find the latest binary in the releases section on Github. Put it on your `PATH` or stick it in the folder of one of your Rails projects.

## Usage

Run `manifold` (if it's on your path) or `./manifold` (if you it's in your project's folder). The program expects you to have a `Procfile.dev` file and will probably crash if you don't. If you do, you should see something like this, with tabs assigned to every process assigned in your `Provfile.dev`:

<img width="1112" alt="Screenshot 2024-11-08 at 21 07 41" src="https://github.com/user-attachments/assets/c087b839-a58a-4256-b40f-9a188cb80bd2">

For the little colored dots next to each tab:

- green means that the tab is currently reading output from the process
- yellow means the process is still running
- red means the process exited with am error (non-zero status code)

I'd love some feedback! Does this work as a drop-in replacement for Overmind? Does it suck?
