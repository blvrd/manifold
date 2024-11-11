# Manifold

Manifold is a process manager. It's directly inspired by [Solo](https://github.com/aarondfrancis/solo), [foreman](https://github.com/ddollar/foreman?tab=readme-ov-file), and [Overmind](https://github.com/DarthSim/overmind). Here's my problem with all of them:

- Solo is for PHP/Laravel only
- Foreman doesn't allow me to interact with processes independent from one another
- Overmind depends on tmux and I'd rather avoid that dependency


## Usage

Run `manifold` (if it's on your path) or `./manifold` (if you it's in your project's folder). The program expects you to have a `Procfile.dev` file and will probably crash if you don't. If you do, you should see something like this, with tabs assigned to every process assigned in your `Procfile.dev`:

<img width="1112" alt="Screenshot 2024-11-08 at 21 07 41" src="https://github.com/user-attachments/assets/c087b839-a58a-4256-b40f-9a188cb80bd2">
