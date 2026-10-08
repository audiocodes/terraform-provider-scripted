# Ask the same program as the resource example a question. It answers
# `op = "data"` with whatever it finds for the given input.
data "scripted_data" "record" {
  program     = ["python3", "${path.module}/manage.py"]
  environment = { STORE_DIR = "${path.module}/store" }

  input = { name = "web-1" }
}

output "exists" {
  value = data.scripted_data.record.output.exists
}
