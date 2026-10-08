terraform {
  required_providers {
    scripted = {
      source = "audiocodes/scripted"
    }
  }
}

provider "scripted" {
  # The program every scripted_resource runs unless it sets its own. Also
  # what `terraform import` and `moved` blocks use, since those have no
  # resource configuration to take a program from.
  program     = ["python3", "manage.py"]
  working_dir = "${path.root}/scripts"

  # Sent to every program run as `provider_input`: the system to talk to,
  # credentials, defaults. Provider configuration never reaches state.
  input = {
    store_dir = "${path.root}/store"
    token     = sensitive(var.api_token)
  }

  # Environment variables work too, for programs that expect them.
  environment = {
    STORE_DIR = "${path.root}/store"
  }
}

variable "api_token" {
  type      = string
  sensitive = true
  default   = ""
}
