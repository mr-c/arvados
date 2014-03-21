#!/usr/bin/env ruby

abort 'Error: Ruby >= 1.9.3 required.' if RUBY_VERSION < '1.9.3'

require 'logger'
require 'trollop'

log = Logger.new STDERR
log.progname = $0.split('/').last

opts = Trollop::options do
  banner ''
  banner "Usage: #{log.progname} " +
    "{user_uuid_or_email} {user_and_repo_name} {vm_uuid}"
  banner ''
  opt :debug, <<-eos
Show debug messages.
  eos
  opt :create, <<-eos
Create a new user with the given email address if an existing user \
is not found.
  eos
  opt :openid_prefix, <<-eos, default: 'https://www.google.com/accounts/o8/id'
If creating a new user record, require authentication from an OpenID \
with this OpenID prefix *and* a matching email address in order to \
claim the account.
  eos
end

log.level = (ENV['DEBUG'] || opts.debug) ? Logger::DEBUG : Logger::WARN
    
if ARGV.count != 3
  Trollop::die "required arguments are missing"
end

user_arg, user_repo_name, vm_uuid = ARGV

require 'arvados'
arv = Arvados.new(api_version: 'v1')

# Look up the given user by uuid or, failing that, email address.
begin
  user = arv.user.get(uuid: user_arg)
rescue Arvados::TransactionFailedError
  found = arv.user.list(where: {email: ARGV[0]})[:items]
         
  if found.count == 0 
    if !user_arg.match(/\w\@\w+\.\w+/)
      abort "About to create new user, but #{user_arg.inspect} " +
               "does not look like an email address. Stop."
    end
           
    user = arv.user.setup(repo_name: user_repo_name, vm_uuid: vm_uuid, 
        user: {email: user_arg})
    log.info { "created user: " + user[:uuid] }
  elsif found.count != 1
    abort "Found #{found.count} users " +
             "with uuid or email #{user_arg.inspect}. Stop."
  else
    user = found.first
    # Found user. Update ther user links
    user = arv.user.setup(repo_name: user_repo_name, vm_uuid: vm_uuid, 
        user: {email: user[:uuid]})
  end

  puts "USER = #{user.inspect}"
  log.info { "user uuid: " + user[:uuid] }
end
